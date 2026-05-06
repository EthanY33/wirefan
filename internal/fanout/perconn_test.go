package fanout

import (
	"context"
	"sync"
	"testing"

	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/registry"
)

type capSub struct {
	mu       sync.Mutex
	received [][]byte
}

func (s *capSub) Send(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, b)
	return nil
}
func (s *capSub) Close() {}

func TestPerConnFanout(t *testing.T) {
	r := registry.NewSyncMap()
	c := r.GetOrCreate("ch")
	a, b := &capSub{}, &capSub{}
	if err := hub.Subscribe(c, a, 100); err != nil {
		t.Fatal(err)
	}
	if err := hub.Subscribe(c, b, 100); err != nil {
		t.Fatal(err)
	}
	NewPerConn().Broadcast(context.Background(), c, []byte("hi"))
	if len(a.received) != 1 || len(b.received) != 1 {
		t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
	}
}
