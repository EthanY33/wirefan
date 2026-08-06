package fanout

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

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

// TestShardedPoolWorkersExitOnClose proves Close actually tears down the
// worker goroutines. This is the observable half of the server.Run shutdown
// path (which closes the pool after Hub.Drain): the server leak test samples
// its baseline after the workers start, so worker teardown must be proven
// here, where the pool's goroutines are created and destroyed inside the
// measurement window.
func TestShardedPoolWorkersExitOnClose(t *testing.T) {
	base := runtime.NumGoroutine()
	f := NewShardedPool(8)
	if n := runtime.NumGoroutine(); n < base+8 {
		t.Fatalf("expected >= 8 worker goroutines after construction: base=%d now=%d", base, n)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close's wg.Wait returns when each worker has run its deferred Done;
	// the goroutine itself unwinds an instant later, so poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workers did not exit after Close: base=%d now=%d", base, runtime.NumGoroutine())
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
