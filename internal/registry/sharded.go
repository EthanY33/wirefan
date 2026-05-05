package registry

import (
	"hash/fnv"
	"sync"
)

const numShards = 16

type shard struct {
	sync.RWMutex
	chans map[string]*Channel
}

type shardedReg struct{ shards [numShards]*shard }

func NewSharded() Registry {
	r := &shardedReg{}
	for i := range r.shards {
		r.shards[i] = &shard{chans: map[string]*Channel{}}
	}
	return r
}

func (r *shardedReg) shardFor(name string) *shard {
	h := fnv.New32a()
	h.Write([]byte(name))
	return r.shards[h.Sum32()%numShards]
}

func (r *shardedReg) GetOrCreate(name string) *Channel {
	s := r.shardFor(name)
	s.RLock()
	if c, ok := s.chans[name]; ok {
		s.RUnlock()
		return c
	}
	s.RUnlock()
	s.Lock()
	defer s.Unlock()
	if c, ok := s.chans[name]; ok {
		return c
	}
	c := newChannel(name)
	s.chans[name] = c
	return c
}

func (r *shardedReg) Lookup(name string) (*Channel, bool) {
	s := r.shardFor(name)
	s.RLock()
	defer s.RUnlock()
	c, ok := s.chans[name]
	return c, ok
}

func (r *shardedReg) Delete(name string) {
	s := r.shardFor(name)
	s.Lock()
	defer s.Unlock()
	delete(s.chans, name)
}

func (r *shardedReg) Range(fn func(*Channel) bool) {
	for _, s := range r.shards {
		s.RLock()
		for _, c := range s.chans {
			if !fn(c) {
				s.RUnlock()
				return
			}
		}
		s.RUnlock()
	}
}

func (r *shardedReg) Len() int {
	n := 0
	for _, s := range r.shards {
		s.RLock()
		n += len(s.chans)
		s.RUnlock()
	}
	return n
}
