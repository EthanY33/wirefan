package fanout

import (
	"context"
	"hash/fnv"
	"sync"

	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/registry"
)

type job struct {
	ch  *registry.Channel
	msg []byte
}

type ShardedPool struct {
	queues  []chan job
	workers int
	wg      sync.WaitGroup

	// mu guards closed and, held exclusively, guarantees no Broadcast is
	// mid-send while Close closes the queues. Broadcast takes the read side
	// so concurrent broadcasts never serialize against each other; only
	// Close excludes them.
	mu     sync.RWMutex
	closed bool
}

func NewShardedPool(workers int) *ShardedPool {
	p := &ShardedPool{queues: make([]chan job, workers), workers: workers}
	for i := 0; i < workers; i++ {
		p.queues[i] = make(chan job, 1024)
		p.wg.Add(1)
		go p.run(i)
	}
	return p
}

func (p *ShardedPool) run(i int) {
	defer p.wg.Done()
	for j := range p.queues[i] {
		hub.Broadcast(j.ch, j.msg)
	}
}

func (p *ShardedPool) shardFor(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % uint32(p.workers))
}

// Broadcast enqueues the message on the channel's shard. After Close it is
// a no-op: the closed check happens under mu's read side, and Close only
// closes the queues while holding the write side, so a Broadcast racing
// Close can never hit send-on-closed-channel.
func (p *ShardedPool) Broadcast(_ context.Context, c *registry.Channel, msg []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	p.queues[p.shardFor(c.Name)] <- job{ch: c, msg: msg}
}

// Close stops accepting broadcasts, closes the shard queues, and waits for
// the workers to drain what was already enqueued. Idempotent. A Broadcast
// blocked on a full queue holds mu.RLock, which delays Close until the
// workers (still running at that point) have made room and the send
// completed, so nothing enqueued before Close is lost.
func (p *ShardedPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	for _, q := range p.queues {
		close(q)
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}
