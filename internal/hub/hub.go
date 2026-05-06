package hub

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// closer is implemented by any conn that can be told to close its WS frame.
type closer interface {
	CloseFrame(code websocket.StatusCode, reason string)
}

// Hub tracks all open conns and broadcasts close frames on shutdown.
type Hub struct {
	mu    sync.RWMutex
	conns map[closer]struct{}
}

// New returns a Hub with an empty conn set.
func New() *Hub { return &Hub{conns: map[closer]struct{}{}} }

// Add registers c with the Hub.
func (h *Hub) Add(c closer) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

// Remove deregisters c from the Hub.
func (h *Hub) Remove(c closer) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Drain sends close frames to all tracked conns and waits up to grace for them
// to deregister. Returns early if ctx is canceled or the conn count hits 0.
func (h *Hub) Drain(ctx context.Context, grace time.Duration) {
	h.mu.RLock()
	for c := range h.conns {
		c.CloseFrame(websocket.StatusGoingAway, "shutdown")
	}
	h.mu.RUnlock()

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		n := len(h.conns)
		h.mu.RUnlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
