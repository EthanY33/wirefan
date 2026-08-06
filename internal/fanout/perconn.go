package fanout

import (
	"context"

	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/registry"
)

type PerConn struct{}

func NewPerConn() Fanout { return &PerConn{} }

func (PerConn) Broadcast(_ context.Context, c *registry.Channel, msg []byte) {
	hub.Broadcast(c, msg)
}

// Close is a no-op: PerConn broadcasts synchronously and owns no goroutines.
func (PerConn) Close() error { return nil }
