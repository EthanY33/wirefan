package hub

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// trackedConn is a live conn the Hub can close, either with a close
// handshake or immediately.
type trackedConn interface {
	// APIKeyID is the id of the API key the conn was opened with.
	APIKeyID() string
	// CloseFrame closes with code and reason via the close handshake. It can
	// block for several seconds on a peer that never answers.
	CloseFrame(code websocket.StatusCode, reason string)
	// CloseNow tears the conn down without a handshake. It must not block.
	CloseNow()
}

// Hub tracks all open conns and broadcasts close frames on shutdown.
type Hub struct {
	mu    sync.RWMutex
	conns map[trackedConn]struct{}
	// closedKeys maps each key passed to CloseKey to the close it sent. It
	// gains one entry per revoked key and is never pruned: a revoked key
	// cannot be restored, and key ids are small.
	closedKeys map[string]websocket.CloseError
}

// New returns a Hub with an empty conn set.
func New() *Hub {
	return &Hub{conns: map[trackedConn]struct{}{}, closedKeys: map[string]websocket.CloseError{}}
}

// Add registers c with the Hub and reports true, unless CloseKey has already
// run for c's key. Then c is not tracked, and Add reports false along with
// the close CloseKey sent, which the caller must send to c itself. Checking
// under the same lock CloseKey sweeps under means every conn either is in
// that sweep or is refused here: an upgrade that looked its key up just
// before a revoke and got here just after would otherwise stay open.
func (h *Hub) Add(c trackedConn) (websocket.CloseError, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ce, closed := h.closedKeys[c.APIKeyID()]; closed {
		return ce, false
	}
	h.conns[c] = struct{}{}
	return websocket.CloseError{}, true
}

// Remove deregisters c from the Hub.
func (h *Hub) Remove(c trackedConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Len reports how many conns are currently tracked.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// snapshot copies the tracked set so callers can act on it without holding
// the lock (closing conns call Remove, which needs the write lock).
func (h *Hub) snapshot() []trackedConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]trackedConn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

// CloseKey closes every tracked conn opened with API key keyID, using code
// and reason, and returns how many it closed. From then on Add refuses conns
// on keyID for the life of the Hub. The handshakes run in their own
// goroutines so the caller (a revoke request) never waits on a slow peer; a
// conn that ignores the handshake is still torn down once coder/websocket's
// handshake wait expires.
func (h *Hub) CloseKey(keyID string, code websocket.StatusCode, reason string) int {
	h.mu.Lock()
	h.closedKeys[keyID] = websocket.CloseError{Code: code, Reason: reason}
	var matched []trackedConn
	for c := range h.conns {
		if c.APIKeyID() == keyID {
			matched = append(matched, c)
		}
	}
	h.mu.Unlock()
	for _, c := range matched {
		go c.CloseFrame(code, reason)
	}
	return len(matched)
}

// Drain sends a GoingAway close to every tracked conn and waits up to grace,
// bounded by ctx, for them to deregister. The closes run concurrently on a
// snapshot of the set: each handshake can wait seconds on a peer that never
// answers, so running them one at a time under the lock made shutdown grow
// by that timeout per unresponsive peer. Whatever is still tracked when the
// wait ends is force-closed with CloseNow, so Drain returns promptly once
// ctx or grace expires. Returns early when the conn count hits 0.
func (h *Hub) Drain(ctx context.Context, grace time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	for _, c := range h.snapshot() {
		go c.CloseFrame(websocket.StatusGoingAway, "shutdown")
	}

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for h.Len() > 0 {
		select {
		case <-ctx.Done():
			for _, c := range h.snapshot() {
				c.CloseNow()
			}
			return
		case <-tick.C:
		}
	}
}
