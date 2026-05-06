package fanout

import (
	"context"
	"testing"

	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/registry"
)

func TestShardedPoolFanout(t *testing.T) {
	f := NewShardedPool(4)
	r := registry.NewSyncMap()
	c := r.GetOrCreate("ch")
	a, b := &capSub{}, &capSub{}
	if err := hub.Subscribe(c, a, 100); err != nil {
		t.Fatal(err)
	}
	if err := hub.Subscribe(c, b, 100); err != nil {
		t.Fatal(err)
	}
	f.Broadcast(context.Background(), c, []byte("hi"))
	f.Close() // synchronously waits for workers to drain
	if len(a.received) != 1 || len(b.received) != 1 {
		t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
	}
}
