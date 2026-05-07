package registry

import (
	"context"
	"time"
)

// DefaultSweepInterval is how often the registry GC runs by default.
const DefaultSweepInterval = time.Minute

// Sweep removes channels with zero subscribers from r. Returns the count
// removed. Locking strategy: each channel's SubsMu is held while checking
// the subscriber count and marking Deleted; this synchronises with
// hub.Subscribe, which checks Deleted under the same lock and returns
// ErrChannelDeleted on a stale reference. The Subscribe caller then retries
// GetOrCreate to obtain a fresh channel.
//
// Sweep is safe to call concurrently with Subscribe / Unsubscribe / Broadcast.
// It's not safe to call concurrently with itself — the registry has only one
// Sweep loop in production. Tests that need concurrent Sweep should
// synchronise externally.
func Sweep(r Registry) int {
	type doomed struct {
		ch *Channel
	}
	var dead []doomed
	r.Range(func(c *Channel) bool {
		c.SubsMu.Lock()
		if len(c.Subscribers) == 0 {
			c.Deleted.Store(true)
			dead = append(dead, doomed{c})
		}
		c.SubsMu.Unlock()
		return true
	})
	for _, d := range dead {
		r.Delete(d.ch.Name)
	}
	return len(dead)
}

// SweepLoop runs Sweep on a ticker until ctx is canceled. Returns when
// ctx.Done() fires. Intended to be launched as a goroutine from main.
func SweepLoop(ctx context.Context, r Registry, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			Sweep(r)
		}
	}
}
