package fanout

import (
	"context"

	"github.com/EthanY33/wirefan/internal/registry"
)

type Fanout interface {
	Broadcast(ctx context.Context, channel *registry.Channel, msg []byte)
	// Close stops any background workers and waits for queued broadcasts to
	// drain. After Close returns, Broadcast is a safe no-op. Implementations
	// without workers return nil immediately.
	Close() error
}
