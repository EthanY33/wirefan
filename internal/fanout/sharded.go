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

func (p *ShardedPool) Broadcast(_ context.Context, c *registry.Channel, msg []byte) {
	p.queues[p.shardFor(c.Name)] <- job{ch: c, msg: msg}
}

func (p *ShardedPool) Close() {
	for _, q := range p.queues {
		close(q)
	}
	p.wg.Wait()
}
