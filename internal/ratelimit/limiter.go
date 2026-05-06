// Package ratelimit implements a per-key token-bucket rate limiter with
// background eviction of stale entries.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// Limiter enforces a token-bucket rate limit per opaque key (e.g. API key ID).
// Entries idle longer than ttl are evicted by a background GC loop.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*entry
	rps     rate.Limit
	burst   int
	ttl     time.Duration
	quit    chan struct{}
}

// New constructs a Limiter with the given per-key rps/burst budget and the
// stale-entry eviction window ttl. The returned Limiter spawns a goroutine
// that must be stopped with Close.
func New(rps int, burst int, ttl time.Duration) *Limiter {
	l := &Limiter{
		entries: map[string]*entry{},
		rps:     rate.Limit(rps),
		burst:   burst,
		ttl:     ttl,
		quit:    make(chan struct{}),
	}
	go l.gcLoop()
	return l
}

// Allow consumes a token from the bucket associated with key, returning true
// if a token was available.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rps, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	l.mu.Unlock()
	return e.lim.Allow()
}

// peek returns the entry for key without touching lastSeen. Test-only.
func (l *Limiter) peek(key string) (*entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	return e, ok
}

// Close terminates the background GC goroutine. Safe to call exactly once.
func (l *Limiter) Close() { close(l.quit) }

func (l *Limiter) gcLoop() {
	t := time.NewTicker(l.ttl)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.gcOnce()
		case <-l.quit:
			return
		}
	}
}

func (l *Limiter) gcOnce() {
	cutoff := time.Now().Add(-l.ttl)
	l.mu.Lock()
	for k, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, k)
		}
	}
	l.mu.Unlock()
}
