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
	Subscribe(c, a)
	Subscribe(c, b)
	Broadcast(c, []byte("hello"))
	if len(a.received) != 1 || len(b.received) != 1 {
		t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
	}
}

func TestChannelUnsubscribe(t *testing.T) {
	c := registry.NewSyncMap().GetOrCreate("test")
	s := &fakeSub{}
	Subscribe(c, s)
	Unsubscribe(c, s)
	Broadcast(c, []byte("hi"))
	if len(s.received) != 0 {
		t.Fatal("should not receive after unsubscribe")
	}
}
