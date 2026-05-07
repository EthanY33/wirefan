package hub

import (
	"errors"

	"github.com/EthanY33/wirefan/internal/registry"
)

// ErrTooManySubs is returned by Subscribe when the channel already has the
// maximum number of subscribers.
var ErrTooManySubs = errors.New("too many subscribers")

// ErrChannelDeleted is returned by Subscribe when the registry has marked
// the channel for GC between the caller's GetOrCreate and Subscribe. The
// caller should retry: a fresh GetOrCreate will return a new (non-deleted)
// channel for the same name. See registry.Sweep.
var ErrChannelDeleted = errors.New("channel deleted")

// Subscribe adds s to c. Returns ErrTooManySubs if the channel already has max
// subscribers, or ErrChannelDeleted if the channel was swept between lookup
// and this call. The caller-supplied max enforces the per-channel cap (spec:
// 10000 — hardcoded at call sites until flag wiring lands).
func Subscribe(c *registry.Channel, s registry.Subscriber, max int) error {
	c.SubsMu.Lock()
	defer c.SubsMu.Unlock()
	if c.Deleted.Load() {
		return ErrChannelDeleted
	}
	if len(c.Subscribers) >= max {
		return ErrTooManySubs
	}
	c.Subscribers[s] = struct{}{}
	return nil
}

func Unsubscribe(c *registry.Channel, s registry.Subscriber) {
	c.SubsMu.Lock()
	delete(c.Subscribers, s)
	c.SubsMu.Unlock()
}

// Broadcast iterates subscribers under per-channel mutex (FIFO ordering).
func Broadcast(c *registry.Channel, msg []byte) {
	c.BroadcastMu.Lock()
	defer c.BroadcastMu.Unlock()
	c.SubsMu.RLock()
	subs := make([]registry.Subscriber, 0, len(c.Subscribers))
	for s := range c.Subscribers {
		subs = append(subs, s)
	}
	c.SubsMu.RUnlock()
	for _, s := range subs {
		_ = s.Send(msg) // policy resolution lives at the conn layer
	}
}

func SubscriberCount(c *registry.Channel) int {
	c.SubsMu.RLock()
	defer c.SubsMu.RUnlock()
	return len(c.Subscribers)
}
