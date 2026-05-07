package registry

import (
	"sync"
	"sync/atomic"
)

type Channel struct {
	Name        string
	SubsMu      sync.RWMutex
	Subscribers map[Subscriber]struct{}

	// Deleted is set true by Sweep when the channel has zero subscribers
	// and is being removed from the registry. Subscribers acquired through
	// GetOrCreate must verify Deleted is false under SubsMu before modifying
	// Subscribers; otherwise the new entry would land on an orphaned channel
	// reference and never receive broadcasts. See registry/sweep.go.
	Deleted atomic.Bool
}

type Subscriber interface {
	Send([]byte) error
	Close()
}

func newChannel(name string) *Channel {
	return &Channel{Name: name, Subscribers: map[Subscriber]struct{}{}}
}

type Registry interface {
	GetOrCreate(name string) *Channel
	Lookup(name string) (*Channel, bool)
	Delete(name string)
	Range(fn func(*Channel) bool)
	Len() int
}
