package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// stuckConn models a peer that ignores the close handshake: CloseFrame blocks
// until the conn is force-closed, and the force close deregisters it the way
// conn.Run's teardown does.
type stuckConn struct {
	h      *Hub
	key    string
	frames atomic.Int32
	code   atomic.Int32
	forced chan struct{}
	once   sync.Once
}

func newStuckConn(h *Hub) *stuckConn { return newKeyedStuckConn(h, "key") }

func newKeyedStuckConn(h *Hub, key string) *stuckConn {
	c := &stuckConn{h: h, key: key, forced: make(chan struct{})}
	h.Add(c)
	return c
}

func (s *stuckConn) APIKeyID() string { return s.key }

func (s *stuckConn) CloseFrame(code websocket.StatusCode, _ string) {
	s.code.Store(int32(code))
	s.frames.Add(1)
	<-s.forced
}

func (s *stuckConn) CloseNow() {
	s.once.Do(func() {
		close(s.forced)
		s.h.Remove(s)
	})
}

// TestDrainForceClosesWhenCtxExpires is the G3 regression. Drain used to call
// CloseFrame on each conn in turn while holding the read lock and never looked
// at ctx, so every peer that ignored the handshake added its full close
// timeout to shutdown. Drain must send every close frame at once, and when
// ctx expires force-close whatever is still open and return.
func TestDrainForceClosesWhenCtxExpires(t *testing.T) {
	h := New()
	conns := []*stuckConn{newStuckConn(h), newStuckConn(h), newStuckConn(h)}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.Drain(ctx, 10*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return within its ctx")
	}

	for i, c := range conns {
		if n := c.frames.Load(); n != 1 {
			t.Errorf("conn %d: %d close frames, want 1", i, n)
		}
		select {
		case <-c.forced:
		default:
			t.Errorf("conn %d was not force-closed", i)
		}
	}
	if n := h.Len(); n != 0 {
		t.Errorf("%d conns still tracked after Drain", n)
	}
}

// TestCloseKeyClosesOnlyThatKey: CloseKey sends the close to every conn of
// the key and to no other, without waiting on peers that never answer.
func TestCloseKeyClosesOnlyThatKey(t *testing.T) {
	h := New()
	a1, a2 := newKeyedStuckConn(h, "a"), newKeyedStuckConn(h, "a")
	b := newKeyedStuckConn(h, "b")
	t.Cleanup(func() { a1.CloseNow(); a2.CloseNow(); b.CloseNow() })

	done := make(chan int, 1)
	go func() { done <- h.CloseKey("a", websocket.StatusPolicyViolation, "key revoked") }()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("CloseKey closed %d conns, want 2", n)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseKey blocked on unresponsive peers")
	}

	deadline := time.Now().Add(time.Second)
	for a1.frames.Load() == 0 || a2.frames.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("close frames never sent to the key's conns")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, c := range []*stuckConn{a1, a2} {
		if got := websocket.StatusCode(c.code.Load()); got != websocket.StatusPolicyViolation {
			t.Errorf("close code %d, want 1008", got)
		}
	}
	if n := b.frames.Load(); n != 0 {
		t.Errorf("conn on another key got %d close frames", n)
	}
}

// TestDrainGraceBoundsWait: grace caps the wait even when ctx has no
// deadline, with the same force close at the end.
func TestDrainGraceBoundsWait(t *testing.T) {
	h := New()
	c := newStuckConn(h)
	done := make(chan struct{})
	go func() {
		h.Drain(context.Background(), 200*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return within its grace period")
	}
	select {
	case <-c.forced:
	default:
		t.Error("conn was not force-closed at the end of grace")
	}
}
