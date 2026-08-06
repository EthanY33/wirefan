package fanout

import (
	"context"

	"github.com/EthanY33/wirefan/internal/registry"
)

type Fanout interface {
	Broadcast(ctx context.Context, channel *registry.Channel, msg []byte)
	// Close stops any background workers and waits for queued broadcasts to
	// drain. ShardedPool makes Broadcast a no-op after Close; PerConn owns no
	// workers, so its Close returns nil immediately and Broadcast keeps
	// delivering synchronously. Callers must stop publishing before relying
	// on Close as a delivery barrier.
	Close() error
}
