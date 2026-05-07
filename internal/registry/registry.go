package registry

import "sync"

type Channel struct {
	Name        string
	SubsMu      sync.RWMutex
	Subscribers map[Subscriber]struct{}
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
