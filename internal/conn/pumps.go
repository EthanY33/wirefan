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

	// Ping blocks until readPump's Read sees the pong, so its pongWait
	// context bounds both the write and the peer's round trip. It runs in
	// its own goroutine: called from this loop it kept the loop off c.send
	// for the whole round trip, and a peer that read every frame but took
	// a moment to answer a ping had its queue fill and was closed as a slow
	// consumer. At most one ping is in flight (pinging), and pong carries
	// its result. The goroutine never outlives writePump: the deferred func
	// cancels the ping and waits for it.
	pingCtx, stopPing := context.WithCancel(ctx)
	pong := make(chan error, 1)
	pinging := false
	defer func() {
		stopPing()
		if pinging {
			<-pong
		}
	}()

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
			if pinging {
				// The last ping is still waiting for its pong (only when
				// pingInterval is shorter than pongWait). Its own deadline
				// decides the conn; a second ping would add nothing.
				continue
			}
			pinging = true
			go func() {
				pctx, cancel := context.WithTimeout(pingCtx, wait)
				defer cancel()
				pong <- c.ws.Ping(pctx)
			}()
		case err := <-pong:
			pinging = false
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
