package registry

import (
	"sync"
	"sync/atomic"
)

type Channel struct {
	Name        string
	SubsMu      sync.RWMutex
	Subscribers map[Subscriber]struct{}

	// Deleted is set true by Sweep when the channel has zero subscribers,
	// in the same SubsMu critical section that removes it from the
	// registry, so a channel marked Deleted is never handed out again.
	// Subscribers acquired through GetOrCreate must verify Deleted is false
	// under SubsMu before modifying Subscribers; otherwise the new entry
	// would land on an orphaned channel reference and never receive
	// broadcasts. See registry/sweep.go.
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
	// CompareAndDelete removes name only while it still maps to c and
	// reports whether it did, so removing a dead channel can never evict a
	// fresh one created under the same name.
	CompareAndDelete(name string, c *Channel) bool
	// Range calls fn for each channel until fn returns false. fn may call
	// back into the registry: Sweep removes channels from inside it.
	Range(fn func(*Channel) bool)
	Len() int
}
