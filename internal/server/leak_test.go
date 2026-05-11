package server

import (
	"context"
	"net/http/httptest"
	"runtime"
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

func TestNoGoroutineLeakAfterChurn(t *testing.T) {
	// Set up handler dependencies first so their persistent goroutines (rate-limiter
	// gcLoop, etc) are part of the baseline rather than counted as leaks.
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

	// Warm up: dial once and let it close so initial httptest goroutines are spun up
	// and the baseline includes them. Otherwise the baseline is captured too early
	// and post-churn count drifts.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		cancel()
		if err == nil {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
	}
	// Let the warm-up conn fully unwind before sampling baseline
	time.Sleep(100 * time.Millisecond)

	base := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				return
			}
			_ = c.Close(websocket.StatusNormalClosure, "")
		}()
	}
	wg.Wait()

	// Allow ws server-side goroutines to drain.
	//
	// Tolerance note: per-conn goroutines (read/write pumps) would produce
	// thousands of extra entries if leaking — 2 goroutines × 1000 conns.
	// The threshold below catches real leaks while tolerating httptest /
	// coder-websocket / net.http background bookkeeping that varies by
	// platform (Windows vs Linux, race vs no-race, Go version). Linux CI
	// commonly retains ~6 extra after churn; Windows local typically <=2.
	const tolerance = 30
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+tolerance {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak suspected: base=%d after=%d (tolerance %d; real per-conn leak would be ~2000)",
		base, runtime.NumGoroutine(), tolerance)
}
