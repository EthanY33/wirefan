package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/fanout"
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
	h := NewUpgradeHandler(s, []string{"*"}, registry.NewSyncMap(), "test-signing-secret", fanout.NewPerConn(), ratelimit.New())
	srv := httptest.NewServer(h)
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func newTestUpgrader(t *testing.T) http.Handler {
	return NewUpgradeHandler(store.NewMemory(), []string{"*"}, registry.NewSyncMap(), "test-signing-secret", fanout.NewPerConn(), ratelimit.New())
}
