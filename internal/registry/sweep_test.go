package registry

import (
	"context"
	"testing"
	"time"
)

type fakeSub struct{}

func (fakeSub) Send([]byte) error { return nil }
func (fakeSub) Close()            {}

func TestSweepRemovesEmptyChannels(t *testing.T) {
	r := NewSyncMap()
	c1 := r.GetOrCreate("empty-1")
	c2 := r.GetOrCreate("empty-2")
	c3 := r.GetOrCreate("has-subs")

	// Add a subscriber to c3 only.
	s := fakeSub{}
	c3.SubsMu.Lock()
	c3.Subscribers[s] = struct{}{}
	c3.SubsMu.Unlock()

	if got, want := r.Len(), 3; got != want {
		t.Fatalf("pre-sweep Len = %d, want %d", got, want)
	}
	swept := Sweep(r)
	if swept != 2 {
		t.Errorf("Sweep returned %d, want 2", swept)
	}
	if got, want := r.Len(), 1; got != want {
		t.Errorf("post-sweep Len = %d, want %d", got, want)
	}
	if !c1.Deleted.Load() || !c2.Deleted.Load() {
		t.Error("expected empty channels to have Deleted=true")
	}
	if c3.Deleted.Load() {
		t.Error("expected non-empty channel to remain non-deleted")
	}
}

func TestSweepLoopHonoursContextCancel(t *testing.T) {
	r := NewSyncMap()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		SweepLoop(ctx, r, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SweepLoop did not return within 1s of ctx cancel")
	}
}

func TestSweepDeletedChannelTriggersFreshGetOrCreate(t *testing.T) {
	r := NewSyncMap()
	c1 := r.GetOrCreate("foo")
	if Sweep(r) != 1 {
		t.Fatal("expected 1 swept channel")
	}
	c2 := r.GetOrCreate("foo")
	if c1 == c2 {
		t.Error("expected GetOrCreate to return a fresh channel after Sweep deletion")
	}
	if c2.Deleted.Load() {
		t.Error("fresh channel should not be marked Deleted")
	}
}
