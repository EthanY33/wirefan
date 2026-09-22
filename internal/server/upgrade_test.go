package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/store"
	"github.com/coder/websocket"
)

func TestUpgradeRequiresKey(t *testing.T) {
	h := newTestUpgrader(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/connect")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
}

func TestUpgradeSucceeds(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-signing-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            hub.New(),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func newTestUpgrader(t *testing.T) http.Handler {
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	return NewUpgradeHandler(UpgradeDeps{
		Store:          store.NewMemory(),
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-signing-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            hub.New(),
	})
}

func TestParseTrustedProxies(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"127.0.0.1", 1},
		{"127.0.0.1/32", 1},
		{"10.0.0.0/8,192.168.0.0/16", 2},
		{"127.0.0.1, ::1", 2},
		{"  not-a-cidr  ", 0},
		{"10.0.0.0/8,bogus,192.168.0.0/16", 2}, // bad entry dropped
	}
	for _, tt := range tests {
		got := parseTrustedProxies(tt.in)
		if len(got) != tt.want {
			t.Errorf("parseTrustedProxies(%q) = %d prefixes, want %d", tt.in, len(got), tt.want)
		}
	}
}

func TestClientIPNoTrust(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := clientIP(r, nil); got != "203.0.113.5" {
		t.Errorf("untrusted: got %q, want %q (XFF must be ignored without trust)", got, "203.0.113.5")
	}
}

func TestClientIPTrustedProxyPicksRightmostUntrusted(t *testing.T) {
	trusted := parseTrustedProxies("127.0.0.1/32,10.0.0.0/8")
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			"loopback proxy + single client",
			"127.0.0.1:8000", "203.0.113.5", "203.0.113.5",
		},
		{
			"chain of trusted proxies — first untrusted is the client",
			"127.0.0.1:8000", "203.0.113.5, 10.0.0.5, 127.0.0.1", "203.0.113.5",
		},
		{
			"client tries to forge — first untrusted hop wins",
			"127.0.0.1:8000", "1.2.3.4, 9.9.9.9, 10.0.0.5", "9.9.9.9",
		},
		{
			"all hops trusted — fall back to leftmost",
			"127.0.0.1:8000", "10.0.0.1, 127.0.0.1, 10.0.0.2", "10.0.0.1",
		},
		{
			"untrusted source ignores XFF",
			"203.0.113.5:1234", "1.2.3.4", "203.0.113.5",
		},
		{
			"IPv6 loopback proxy",
			"[::1]:8000", "203.0.113.5", "203.0.113.5",
		},
	}
	// Add ::1 to trusted for the IPv6 case.
	trusted = append(trusted, mustPrefix(t, "::1/128"))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			got := clientIP(r, trusted)
			if got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseIPCap(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultIPCap},
		{"500", 500},
		{" 1000 ", 1000},
		{"0", defaultIPCap},
		{"-5", defaultIPCap},
		{"lots", defaultIPCap},
	}
	for _, c := range cases {
		if got := parseIPCap(c.raw); got != c.want {
			t.Errorf("parseIPCap(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestNewUpgradeHandlerHonorsIPCapEnv(t *testing.T) {
	t.Setenv("WIREFAN_IP_CAP", "12345")
	h := NewUpgradeHandler(UpgradeDeps{})
	if h.ipCap != 12345 {
		t.Fatalf("ipCap = %d, want 12345", h.ipCap)
	}
}

// TestIPCapKeysIPv6By64 proves the per-IP connection cap counts an IPv6
// client by its /64 while IPv4 stays keyed by the full address. One
// subscriber line is routinely delegated a whole /64 and can source
// connections from any address in it, so keying on the full IPv6 address
// let a single client rotate addresses and never hit the cap. Addresses
// reach the handler through X-Forwarded-For from a trusted loopback proxy,
// the same path production uses behind a reverse proxy.
func TestIPCapKeysIPv6By64(t *testing.T) {
	t.Setenv("WIREFAN_IP_CAP", "1")
	t.Setenv("WIREFAN_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	ctx := context.Background()
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(ctx, "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-signing-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            hub.New(),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close) // runs after the per-socket cleanups below
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	// dialFrom opens a socket that claims to come from client. Accepted
	// sockets stay open until the test ends so they keep holding the cap.
	dialFrom := func(client string) (accepted bool, status int) {
		t.Helper()
		hdr := http.Header{}
		hdr.Set("X-Forwarded-For", client)
		c, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if res != nil {
			status = res.StatusCode
		}
		if err != nil {
			return false, status
		}
		t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
		return true, status
	}
	mustAccept := func(client, why string) {
		t.Helper()
		if ok, status := dialFrom(client); !ok {
			t.Fatalf("%s: dial from %s refused (status %d), want accepted", why, client, status)
		}
	}
	mustCap := func(client, why string) {
		t.Helper()
		ok, status := dialFrom(client)
		if ok {
			t.Fatalf("%s: dial from %s accepted, want the per-IP cap to refuse it", why, client)
		}
		if status != http.StatusTooManyRequests {
			t.Fatalf("%s: dial from %s got status %d, want %d", why, client, status, http.StatusTooManyRequests)
		}
	}

	mustAccept("2001:db8:1:2::1", "first IPv6 client")
	mustCap("2001:db8:1:2:ffff:ffff:ffff:9", "another address in the same /64")
	mustAccept("2001:db8:1:3::1", "a different /64 is a different client")

	mustAccept("203.0.113.5", "first IPv4 client")
	mustAccept("203.0.113.6", "an IPv4 neighbor is a different client")
	mustCap("203.0.113.5", "the same IPv4 address again")
	// An IPv4-mapped IPv6 address is that IPv4 client. Taking its /64
	// instead would put every mapped client in the one ::/64 bucket.
	mustCap("::ffff:203.0.113.5", "the IPv4-mapped form of a capped IPv4 address")
}

func TestIPCapKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"203.0.113.5", "203.0.113.5"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"::ffff:203.0.113.5", "203.0.113.5"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, c := range cases {
		if got := ipCapKey(c.in); got != c.want {
			t.Errorf("ipCapKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}
