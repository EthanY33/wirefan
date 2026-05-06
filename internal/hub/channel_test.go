package hub

import (
	"errors"
	"sync"
	"testing"

	"github.com/EthanY33/wirefan/internal/registry"
)

type fakeSub struct {
	mu       sync.Mutex
	received [][]byte
	failNext bool
}

func (f *fakeSub) Send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("forced")
	}
	f.received = append(f.received, b)
	return nil
}
func (f *fakeSub) Close() {}

func TestChannelSubscribeBroadcast(t *testing.T) {
	c := registry.NewSyncMap().GetOrCreate("test")
	a, b := &fakeSub{}, &fakeSub{}
	if err := Subscribe(c, a, 100); err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	if err := Subscribe(c, b, 100); err != nil {
		t.Fatalf("subscribe b: %v", err)
	}
	Broadcast(c, []byte("hello"))
	if len(a.received) != 1 || len(b.received) != 1 {
		t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
	}
}

func TestChannelUnsubscribe(t *testing.T) {
	c := registry.NewSyncMap().GetOrCreate("test")
	s := &fakeSub{}
	if err := Subscribe(c, s, 100); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	Unsubscribe(c, s)
	Broadcast(c, []byte("hi"))
	if len(s.received) != 0 {
		t.Fatal("should not receive after unsubscribe")
	}
}

func TestSubscribeRespectsMax(t *testing.T) {
	c := registry.NewSyncMap().GetOrCreate("test")
	a, b := &fakeSub{}, &fakeSub{}
	if err := Subscribe(c, a, 1); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if err := Subscribe(c, b, 1); err == nil {
		t.Fatal("expected ErrTooManySubs")
	} else if !errors.Is(err, ErrTooManySubs) {
		t.Fatalf("expected ErrTooManySubs, got %v", err)
	}
}
