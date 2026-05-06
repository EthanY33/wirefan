package conn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
)

const (
	sendChanSize    = 64
	pingInterval    = 30 * time.Second
	readDeadline    = 60 * time.Second
	writeDeadline   = 10 * time.Second
	protocolVersion = "v1"
)

// ErrSlowConsumer is returned by Conn.Send when the send buffer is full.
// Task 15's backpressure policy hooks here.
var ErrSlowConsumer = errors.New("slow consumer")

type Conn struct {
	ws            *websocket.Conn
	socketID      string
	send          chan []byte
	registry      registry.Registry
	signingSecret string
	subs          map[string]*registry.Channel
	subsMu        sync.Mutex
}

// Run owns the conn for its lifetime. Returns when ctx is canceled or peer disconnects.
func Run(ctx context.Context, ws *websocket.Conn, socketID string, reg registry.Registry, signingSecret string) error {
	c := &Conn{
		ws:            ws,
		socketID:      socketID,
		send:          make(chan []byte, sendChanSize),
		registry:      reg,
		signingSecret: signingSecret,
		subs:          map[string]*registry.Channel{},
	}

	hello, _ := json.Marshal(map[string]string{
		"type":      "connected",
		"socket_id": socketID,
		"version":   protocolVersion,
	})
	select {
	case c.send <- hello:
	default:
		return ws.Close(websocket.StatusInternalError, "send chan full at start")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, 2)
	go func() { errc <- c.writePump(runCtx) }()
	go func() { errc <- c.readPump(runCtx) }()
	err := <-errc
	cancel()
	<-errc
	if err != nil {
		slog.Debug("conn closed", "socket_id", socketID, "err", err)
	}

	// Cleanup: unsubscribe from all channels on exit so registry doesn't leak
	// references to a dead conn.
	c.subsMu.Lock()
	subs := c.subs
	c.subs = nil
	c.subsMu.Unlock()
	for name, ch := range subs {
		hub.Unsubscribe(ch, c)
		if hub.SubscriberCount(ch) == 0 {
			c.registry.Delete(name)
		}
	}

	return err
}
