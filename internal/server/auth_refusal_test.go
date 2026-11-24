package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// These tests cover the refusing side of the credential checks: each one
// asserts that a bad credential is rejected, so that dropping a clause from
// a guard fails a test instead of passing silently.

func newRestMux(s store.Store) *httptest.Server {
	mux := http.NewServeMux()
	NewRestHandler(s, "admin-tok", "server-signing-secret", hub.New()).Register(mux)
	return httptest.NewServer(mux)
}

func signStatus(t *testing.T, srv *httptest.Server, authz string) int {
	t.Helper()
	b := `{"socket_id":"` + testSocketID + `","channel":"private-room"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/sign", strings.NewReader(b))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestAuthSignRefusesBadCredentials(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	good, _ := s.CreateKey(context.Background(), "good", auth.HashSecret(secret))
	revoked, _ := s.CreateKey(context.Background(), "revoked", auth.HashSecret(secret))
	if err := s.RevokeKey(context.Background(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	srv := newRestMux(s)
	defer srv.Close()

	// Sanity: the good credentials are accepted, so the 401s below come
	// from the credential checks and not from a malformed request.
	if got := signStatus(t, srv, "Bearer "+good.ID+":"+secret); got != http.StatusOK {
		t.Fatalf("valid credentials: want 200, got %d", got)
	}

	cases := []struct{ name, authz string }{
		{"wrong secret", "Bearer " + good.ID + ":not-the-secret"},
		{"unknown key id", "Bearer 01HX-no-such-key:" + secret},
		{"revoked key", "Bearer " + revoked.ID + ":" + secret},
		{"no colon", "Bearer " + good.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signStatus(t, srv, tc.authz); got != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", got)
			}
		})
	}
}

func TestRequireAdminRefusesWrongToken(t *testing.T) {
	srv := newRestMux(store.NewMemory())
	defer srv.Close()

	cases := []struct {
		name, authz string
		want        int
	}{
		{"right token", "Bearer admin-tok", http.StatusOK},
		{"wrong token, same length", "Bearer admin-tox", http.StatusUnauthorized},
		{"wrong token, shorter", "Bearer x", http.StatusUnauthorized},
		{"wrong token, longer", "Bearer admin-tok-and-more", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"wrong scheme", "Basic admin-tok", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
			req.Header.Set("Authorization", tc.authz)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("want %d, got %d", tc.want, res.StatusCode)
			}
		})
	}
}

func newUpgradeDeps(t *testing.T, s store.Store, origins []string, h *hub.Hub) UpgradeDeps {
	t.Helper()
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	return UpgradeDeps{
		Store:          s,
		AllowedOrigins: origins,
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-signing-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	}
}

// TestUpgradeRefusesUnknownAndRevokedKey uses a fresh hub, which models a
// restart: Hub.closedKeys is in memory and empty, so the store check in the
// upgrade handler is the only guard against a revoked key.
func TestUpgradeRefusesUnknownAndRevokedKey(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	good, _ := s.CreateKey(context.Background(), "good", auth.HashSecret(secret))
	revoked, _ := s.CreateKey(context.Background(), "revoked", auth.HashSecret(secret))
	if err := s.RevokeKey(context.Background(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewUpgradeHandler(newUpgradeDeps(t, s, []string{"*"}, hub.New())))
	defer srv.Close()

	status := func(keyID string) int {
		res, err := http.Get(srv.URL + "/v1/connect?key=" + keyID)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if got := status("01HX-no-such-key"); got != http.StatusUnauthorized {
		t.Fatalf("unknown key: want 401, got %d", got)
	}
	if got := status(revoked.ID); got != http.StatusUnauthorized {
		t.Fatalf("revoked key: want 401, got %d", got)
	}
	// The good key gets past the key check: a plain GET is not a WebSocket
	// upgrade, so the library answers something other than 401.
	if got := status(good.ID); got == http.StatusUnauthorized {
		t.Fatalf("valid key: got 401")
	}
}

func TestUpgradeEnforcesAllowedOrigins(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	srv := httptest.NewServer(NewUpgradeHandler(newUpgradeDeps(t, s, []string{"app.example"}, hub.New())))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	dial := func(origin string) (*http.Response, error) {
		c, res, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{origin}},
		})
		if err == nil {
			_ = c.CloseNow()
		}
		return res, err
	}

	res, err := dial("https://evil.example")
	if err == nil {
		t.Fatal("origin not in the allowlist: dial succeeded")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("origin not in the allowlist: want 403, got %v", res)
	}

	res, err = dial("https://app.example")
	if err != nil {
		t.Fatalf("allowed origin: dial failed: %v", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("allowed origin: want 101, got %d", res.StatusCode)
	}
}

// TestServerNewRejectsReplayedToken goes through server.New so it fails if
// the replay cache is not wired into the upgrade handler: a subscribe token
// is single use, and reusing it after an unsubscribe must be refused.
func TestServerNewRejectsReplayedToken(t *testing.T) {
	st := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := st.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	s := New(Config{AllowedOrigins: []string{"*"}}, Deps{
		Store:         st,
		AdminToken:    "admin-tok",
		Registry:      registry.NewSyncMap(),
		SigningSecret: "test-secret",
		Fanout:        fanout.NewPerConn(),
		RateLimit:     rl,
		Policy:        conn.PolicyDisconnect{},
		Hub:           hub.New(),
	})
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http", "ws", 1)+"/v1/connect?key="+k.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		SocketID string `json:"socket_id"`
	}
	if err := json.Unmarshal(raw, &hello); err != nil || hello.SocketID == "" {
		t.Fatalf("bad hello %s: %v", raw, err)
	}

	const ch = "private-room"
	tok, err := auth.SignToken("test-secret", hello.SocketID, ch, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	send := func(frame map[string]string) map[string]any {
		b, _ := json.Marshal(frame)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad frame %s: %v", raw, err)
		}
		return out
	}

	if got := send(map[string]string{"type": "subscribe", "channel": ch, "token": tok}); got["type"] != "subscribed" {
		t.Fatalf("first subscribe: want subscribed, got %v", got)
	}
	if got := send(map[string]string{"type": "unsubscribe", "channel": ch}); got["type"] != "unsubscribed" {
		t.Fatalf("unsubscribe: want unsubscribed, got %v", got)
	}
	got := send(map[string]string{"type": "subscribe", "channel": ch, "token": tok})
	if got["type"] != "error" || got["code"] != "AUTH_REPLAYED" {
		t.Fatalf("reused token: want AUTH_REPLAYED, got %v", got)
	}
}
