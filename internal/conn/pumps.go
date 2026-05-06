package conn

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
)

func (c *Conn) writePump(ctx context.Context) error {
	ticker := time.NewTicker(pingInterval)
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
			pctx, cancel := context.WithTimeout(ctx, writeDeadline)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

func (c *Conn) readPump(ctx context.Context) error {
	c.ws.SetReadLimit(64 * 1024)
	for {
		rctx, cancel := context.WithTimeout(ctx, readDeadline)
		_, raw, err := c.ws.Read(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return c.ws.Close(websocket.StatusGoingAway, "")
			}
			return err
		}
		c.handle(raw)
	}
}
