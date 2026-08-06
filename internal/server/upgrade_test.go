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

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}
