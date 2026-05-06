package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/registry"
)

type captureSub struct {
	mu       sync.Mutex
	received [][]byte
}

func (s *captureSub) Send(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	s.received = append(s.received, cp)
	return nil
}

func (s *captureSub) Close() {}

func (s *captureSub) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

func TestPublishStatsLoop(t *testing.T) {
	r := registry.NewSyncMap()
	ch := r.GetOrCreate("_wirefan-stats")
	sub := &captureSub{}
	if err := Subscribe(ch, sub, 100); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snap := func() map[string]int64 {
		return map[string]int64{"connections": 42, "messages": 7}
	}

	done := make(chan struct{})
	go func() {
		PublishStatsLoop(ctx, r, 10*time.Millisecond, snap)
		close(done)
	}()

	// Wait for at least 1 message
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sub.Count() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sub.Count() < 1 {
		t.Fatal("expected at least one stats message within 2s")
	}

	// Verify shape
	sub.mu.Lock()
	raw := sub.received[0]
	sub.mu.Unlock()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "event" || got["channel"] != "_wirefan-stats" {
		t.Fatalf("unexpected envelope: %+v", got)
	}
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data not map: %T", got["data"])
	}
	if data["connections"] != float64(42) { // JSON numbers are float64
		t.Errorf("connections=%v want 42", data["connections"])
	}

	cancel()
	<-done
}
