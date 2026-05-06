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
