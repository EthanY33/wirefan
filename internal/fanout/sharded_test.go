package fanout

import (
	"context"
	"sync"
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
	_ = f.Close() // synchronously waits for workers to drain
	if len(a.received) != 1 || len(b.received) != 1 {
		t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
	}
}

// TestShardedPoolBroadcastCloseRace hammers Broadcast from many goroutines
// while Close runs concurrently. Any send-on-closed-channel panics the run;
// under -race this also proves the closed flag is properly synchronized.
func TestShardedPoolBroadcastCloseRace(t *testing.T) {
	f := NewShardedPool(4)
	r := registry.NewSyncMap()
	c := r.GetOrCreate("race-ch")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				f.Broadcast(context.Background(), c, []byte("m"))
			}
		}()
	}
	close(start)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	// After Close, Broadcast must be a no-op and Close idempotent.
	f.Broadcast(context.Background(), c, []byte("late"))
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
