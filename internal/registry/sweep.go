package registry

import (
	"context"
	"time"
)

// DefaultSweepInterval is how often the registry GC runs by default.
const DefaultSweepInterval = time.Minute

// Sweep removes channels with zero subscribers from r. Returns the count
// removed. Locking strategy: each channel's SubsMu is held while checking
// the subscriber count, removing the channel from the registry, and
// marking it Deleted; this synchronises with hub.Subscribe, which checks
// Deleted under the same lock and returns ErrChannelDeleted on a stale
// reference. The Subscribe caller then retries GetOrCreate, which by then
// can only return a fresh channel.
//
// The removal has to happen inside that critical section. Marking channels
// Deleted during Range and deleting them only after Range finished left
// every dead channel in the registry for the rest of the pass, so
// GetOrCreate kept handing it back and a racing subscribe could exhaust
// its retries and fail. CompareAndDelete removes the entry only while it
// still maps to this channel, so a fresh channel created under the same
// name is never evicted, and a second concurrent Sweep finds nothing left
// to remove.
//
// Sweep is safe to call concurrently with Subscribe / Unsubscribe / Broadcast.
func Sweep(r Registry) int {
	removed := 0
	r.Range(func(c *Channel) bool {
		c.SubsMu.Lock()
		if len(c.Subscribers) == 0 {
			if r.CompareAndDelete(c.Name, c) {
				removed++
			}
			// Mark after removal: a GetOrCreate that still found c
			// linearizes before the removal, when c was live, and its
			// subscribe then sees Deleted under SubsMu and retries.
			c.Deleted.Store(true)
		}
		c.SubsMu.Unlock()
		return true
	})
	return removed
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
