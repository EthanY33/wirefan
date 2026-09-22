package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestCreateAndListKeys(t *testing.T) {
	s := store.NewMemory()
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// create
	body := bytes.NewBufferString(`{"name":"app1"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/keys", body)
	req.Header.Set("Authorization", "Bearer admin-tok")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", res.StatusCode)
	}
	var created struct{ ID, Secret string }
	_ = json.NewDecoder(res.Body).Decode(&created)
	if created.ID == "" || created.Secret == "" {
		t.Fatal("expected id and secret")
	}

	// list
	req2, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
	req2.Header.Set("Authorization", "Bearer admin-tok")
	res2, _ := http.DefaultClient.Do(req2)
	body2, _ := io.ReadAll(res2.Body)
	if !bytes.Contains(body2, []byte(created.ID)) {
		t.Fatalf("list missing id: %s", body2)
	}
}

func TestRevokeKey(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/keys/"+k.ID, nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", res.StatusCode)
	}

	req2, _ := http.NewRequest("DELETE", srv.URL+"/v1/keys/01HX-does-not-exist", nil)
	req2.Header.Set("Authorization", "Bearer admin-tok")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke missing: want 404, got %d", res2.StatusCode)
	}
}

// TestRevokeKeyClosesLiveConns is the G6 regression. Revoking a key used to
// stop only new upgrades and new sign requests; sockets already open with
// the key kept working. After DELETE /v1/keys/{id} returns, every live conn
// opened with that key must be closed with 1008 "key revoked", and conns on
// other keys must be left alone.
func TestRevokeKeyClosesLiveConns(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	revoked, _ := s.CreateKey(context.Background(), "revoked", auth.HashSecret(secret))
	kept, _ := s.CreateKey(context.Background(), "kept", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()

	mux := http.NewServeMux()
	NewRestHandler(s, "admin-tok", "test-signing-secret", h).Register(mux)
	mux.Handle("/v1/connect", NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-signing-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key="

	dial := func(keyID string) *websocket.Conn {
		c, _, err := websocket.Dial(context.Background(), wsBase+keyID, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		if _, _, err := c.Read(context.Background()); err != nil { // hello
			t.Fatalf("hello: %v", err)
		}
		return c
	}
	victims := []*websocket.Conn{dial(revoked.ID), dial(revoked.ID)}
	survivor := dial(kept.ID)

	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/keys/"+revoked.ID, nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", res.StatusCode)
	}

	for i, c := range victims {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, err := c.Read(ctx)
		cancel()
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("conn %d on the revoked key: want a close frame, got %v", i, err)
		}
		if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "key revoked" {
			t.Fatalf("conn %d: want 1008 %q, got %d %q", i, "key revoked", ce.Code, ce.Reason)
		}
	}

	// The other key's conn still round-trips.
	b, _ := json.Marshal(map[string]string{"type": "subscribe", "channel": "public-alive"})
	if err := survivor.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("survivor write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := survivor.Read(ctx)
	if err != nil {
		t.Fatalf("conn on an unrevoked key was closed: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"subscribed"`)) {
		t.Fatalf("survivor: want subscribed ack, got %s", raw)
	}
}

func TestListKeysOmitsSecretHash(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	_, _ = s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("want application/json, got %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if bytes.Contains(bytes.ToLower(body), []byte("secret")) {
		t.Fatalf("list response leaked secret field: %s", body)
	}
}

func TestRequiresAdminBearer(t *testing.T) {
	s := store.NewMemory()
	rest := NewRestHandler(s, "tok", "test-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
}

func TestAuthSign(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "server-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := bytes.NewBufferString(`{"socket_id":"` + testSocketID + `","channel":"private-room"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/sign", body)
	req.Header.Set("Authorization", "Bearer "+k.ID+":"+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	var got struct{ Token string }
	_ = json.NewDecoder(res.Body).Decode(&got)
	if err := auth.VerifyToken("server-signing-secret", testSocketID, "private-room", got.Token); err != nil {
		t.Fatal(err)
	}
}

// testSocketID is a well-formed ULID, the shape /v1/connect hands out.
const testSocketID = "01J8Z4Q0M3W6B9XKAV5T2C7RPD"

// postSign calls /v1/auth/sign with valid key credentials and returns the
// status code and decoded token (empty unless 200).
func postSign(t *testing.T, socketID, channel string) (int, string) {
	t.Helper()
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "server-signing-secret", hub.New())
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, _ := json.Marshal(map[string]string{"socket_id": socketID, "channel": channel})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/sign", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+k.ID+":"+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var got struct{ Token string }
	if res.StatusCode == http.StatusOK {
		_ = json.NewDecoder(res.Body).Decode(&got)
	}
	return res.StatusCode, got.Token
}

// TestAuthSignRejectsFieldShiftSocketID is the endpoint half of the G1
// regression: an app server that forwards a browser-chosen socket_id
// verbatim must not get a token back for "S|private-victim", the input
// that let the returned token be reshaped into one for private-victim.
func TestAuthSignRejectsFieldShiftSocketID(t *testing.T) {
	code, tok := postSign(t, testSocketID+"|private-victim", "private-attacker")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (token %q)", code, tok)
	}
}

func TestAuthSignValidatesInput(t *testing.T) {
	cases := []struct {
		name, socketID, channel string
		want                    int
	}{
		{"private ok", testSocketID, "private-room", http.StatusOK},
		{"presence ok", testSocketID, "presence-room", http.StatusOK},
		{"empty socket_id", "", "private-room", http.StatusBadRequest},
		{"short socket_id", "01HX", "private-room", http.StatusBadRequest},
		{"non-base32 socket_id", "01J8Z4Q0M3W6B9XKAV5T2C7RP!", "private-room", http.StatusBadRequest},
		{"public channel", testSocketID, "public-room", http.StatusBadRequest},
		{"unprefixed channel", testSocketID, "room", http.StatusBadRequest},
		{"reserved channel", testSocketID, "_wirefan-stats", http.StatusBadRequest},
		{"empty channel", testSocketID, "", http.StatusBadRequest},
		{"overlong channel", testSocketID, "private-" + strings.Repeat("x", 200), http.StatusBadRequest},
		{"control char channel", testSocketID, "private-a\nb", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := postSign(t, tc.socketID, tc.channel); code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, code)
			}
		})
	}
}
