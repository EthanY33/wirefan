package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestDrainClosesAllConnections(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()

	upgrader := NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	})
	srv := httptest.NewServer(upgrader)
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	// Open 5 conns; each starts a Run goroutine on the server side
	conns := make([]*websocket.Conn, 0, 5)
	var clientWG sync.WaitGroup
	for i := 0; i < 5; i++ {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Read the connected hello so the server has fully attached
		_, _, _ = c.Read(context.Background())
		conns = append(conns, c)
		clientWG.Add(1)
		go func(c *websocket.Conn) {
			defer clientWG.Done()
			// Read until close
			for {
				if _, _, err := c.Read(context.Background()); err != nil {
					return
				}
			}
		}(c)
	}

	// Allow registration to settle
	time.Sleep(100 * time.Millisecond)

	// Drain
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.Drain(drainCtx, 5*time.Second)
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return within 10s")
	}

	// All client readers should have observed close
	clientDone := make(chan struct{})
	go func() {
		clientWG.Wait()
		close(clientDone)
	}()
	select {
	case <-clientDone:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("client conns did not close within 2s of Drain")
	}

	for _, c := range conns {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
}

// TestDrainNonReadingPeersHonorsCtx is the G3 end-to-end check. A peer that
// never reads never answers the close handshake, so each server-side Close
// sits in coder/websocket's 5 s handshake wait. Drain used to run those one
// at a time under the hub lock, ignoring ctx: five such peers held shutdown
// for about 25 s. Drain must return within its ctx and leave no conn open.
func TestDrainNonReadingPeersHonorsCtx(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()
	srv := httptest.NewServer(NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	const peers = 5
	for i := 0; i < peers; i++ {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		// Read only the hello, then go silent for good.
		if _, _, err := c.Read(context.Background()); err != nil {
			t.Fatalf("hello %d: %v", i, err)
		}
	}
	waitForLen(t, h, peers)

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	h.Drain(drainCtx, 30*time.Second)
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("Drain took %v with a 1 s ctx", elapsed)
	}
	waitForLen(t, h, 0)
}

// waitForLen polls until h tracks exactly n conns, failing after 2 s.
func waitForLen(t *testing.T, h *hub.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.Len() != n {
		if time.Now().After(deadline) {
			t.Fatalf("hub tracks %d conns, want %d", h.Len(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
