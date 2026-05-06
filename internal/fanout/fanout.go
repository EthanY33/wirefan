package fanout

import (
	"context"

	"github.com/EthanY33/wirefan/internal/registry"
)

type Fanout interface {
	Broadcast(ctx context.Context, channel *registry.Channel, msg []byte)
}
