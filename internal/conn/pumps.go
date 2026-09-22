package conn

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
)

func (c *Conn) writePump(ctx context.Context) error {
	// Read the keepalive vars once so a test that shortens them cannot race
	// a pump that is already running.
	interval, wait := pingInterval, pongWait
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-c.send:
			if !ok {
				return c.ws.Close(websocket.StatusNormalClosure, "")
			}
			wctx, cancel := context.WithTimeout(ctx, writeDeadline)
			err := c.ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return err
			}
		case <-ticker.C:
			// Ping blocks until readPump's Read sees the pong, so this
			// bounds both the write and the peer's round trip.
			pctx, cancel := context.WithTimeout(ctx, wait)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// readPump reads on the Run context with no per-Read timeout; liveness is
// writePump's job (see pingInterval). Pongs are consumed inside Read, which
// is what lets writePump's Ping return.
func (c *Conn) readPump(ctx context.Context) error {
	c.ws.SetReadLimit(64 * 1024)
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return c.ws.Close(websocket.StatusGoingAway, "")
			}
			return err
		}
		c.handle(ctx, raw)
	}
}
