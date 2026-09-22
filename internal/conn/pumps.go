package conn

import (
	"context"
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
// is what lets writePump's Ping return. Errors are returned as-is: Run owns
// tearing the socket down once both pumps are done. (A canceled ctx has
// already made coder/websocket close the socket under the in-flight Read,
// so the GoingAway Close that used to live here never reached the peer.)
func (c *Conn) readPump(ctx context.Context) error {
	c.ws.SetReadLimit(64 * 1024)
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		c.handle(ctx, raw)
	}
}
