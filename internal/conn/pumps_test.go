package conn

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
)

// setKeepalive shortens the keepalive timers for one test and restores the
// production values in cleanup. Call it before dialing: writePump reads both
// values once, when it starts.
func setKeepalive(t *testing.T, interval, wait time.Duration) {
	t.Helper()
	oldInterval, oldWait := pingInterval, pongWait
	pingInterval, pongWait = interval, wait
	t.Cleanup(func() { pingInterval, pongWait = oldInterval, oldWait })
}

// setCloseHandshakeTimeout shortens closeHandshakeTimeout for one test. Call
// it before dialing: Run reads it once, when it starts.
func setCloseHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := closeHandshakeTimeout
	closeHandshakeTimeout = d
	t.Cleanup(func() { closeHandshakeTimeout = old })
}

// serveRun serves every dialed WebSocket with its own Run (no parent
// deadline, so only the conn's own logic can end it) and reports the time
// each Run returned on the returned channel.
func serveRun(t *testing.T) (string, <-chan time.Time) {
	t.Helper()
	return serveRunOn(t, hub.New())
}

// serveRunOn is serveRun with every Run tracked by h, under API key
// "test-key". Like server.New it installs WithNetConn, so Run can reach the
// TCP connection.
func serveRunOn(t *testing.T, h *hub.Hub) (string, <-chan time.Time) {
	t.Helper()
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	ended := make(chan time.Time, 8)
	srv := httptest.NewUnstartedServer(websocketHandlerCtx(func(ctx context.Context, c *websocket.Conn) {
		_ = Run(ctx, c, "01HTEST", "test-key", Deps{
			Registry:      registry.NewSyncMap(),
			SigningSecret: "test-signing-secret",
			Fanout:        fanout.NewPerConn(),
			RateLimit:     rl,
			Policy:        PolicyDisconnect{},
			Hub:           h,
		})
		ended <- time.Now()
	}))
	srv.Config.ConnContext = WithNetConn
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http", "ws", 1), ended
}

func dialWS(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// TestKeepaliveIdleReaderStaysConnected is the G2 regression: a client that
// sends nothing but keeps reading (and so answers every ping) must stay
// connected indefinitely. Before the fix readPump wrapped each Read in a
// fixed deadline that pongs never reset, so a healthy idle client was cut
// off after 60 s.
func TestKeepaliveIdleReaderStaysConnected(t *testing.T) {
	setKeepalive(t, 50*time.Millisecond, 250*time.Millisecond)
	wsURL, ended := serveRun(t)
	c := dialWS(t, wsURL)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	// 40 ping rounds, far past the old 2x-pingInterval read deadline.
	select {
	case <-ended:
		t.Fatal("healthy idle client was disconnected")
	case <-time.After(40 * 50 * time.Millisecond):
	}
}

// TestKeepaliveSilentPeerDisconnected proves dead-peer detection now rests on
// the pong wait: a client that never reads never answers a ping, so Run must
// end within about pingInterval + pongWait.
func TestKeepaliveSilentPeerDisconnected(t *testing.T) {
	const interval, wait = 50 * time.Millisecond, 200 * time.Millisecond
	setKeepalive(t, interval, wait)
	wsURL, ended := serveRun(t)
	start := time.Now()
	dialWS(t, wsURL) // never read: pings go unanswered

	select {
	case at := <-ended:
		elapsed := at.Sub(start)
		if elapsed < wait {
			t.Fatalf("closed after %v, before the %v pong wait could expire", elapsed, wait)
		}
		if limit := interval + wait + 1500*time.Millisecond; elapsed > limit {
			t.Fatalf("closed after %v, want within %v", elapsed, limit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silent peer was never disconnected")
	}
}
