package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/store"
)

func TestCreateAndListKeys(t *testing.T) {
	s := store.NewMemory()
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret")
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
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret")
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

func TestListKeysOmitsSecretHash(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	_, _ = s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret")
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
	rest := NewRestHandler(s, "tok", "test-signing-secret")
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
	rest := NewRestHandler(s, "admin-tok", "server-signing-secret")
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
	rest := NewRestHandler(s, "admin-tok", "server-signing-secret")
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
