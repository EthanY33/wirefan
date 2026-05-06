package conn

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/coder/websocket"
)

const (
	sendChanSize    = 64
	pingInterval    = 30 * time.Second
	readDeadline    = 60 * time.Second
	writeDeadline   = 10 * time.Second
	protocolVersion = "v1"
)

type Conn struct {
	ws       *websocket.Conn
	socketID string
	send     chan []byte
}

// Run owns the conn for its lifetime. Returns when ctx is canceled or peer disconnects.
func Run(ctx context.Context, ws *websocket.Conn, socketID string) error {
	c := &Conn{ws: ws, socketID: socketID, send: make(chan []byte, sendChanSize)}

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
	return err
}
