package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
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

// serveSharedRun is serveRun with one registry, hub, fanout and rate limiter
// shared by every dialed conn, so a publish on one conn reaches the
// subscribers on the others (serveRun gives each conn a registry of its
// own). The limits are the production ones. Each conn gets its own socket id.
func serveSharedRun(t *testing.T) string {
	t.Helper()
	reg := registry.NewSyncMap()
	h := hub.New()
	fan := fanout.NewPerConn()
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(websocketHandlerCtx(func(ctx context.Context, c *websocket.Conn) {
		_ = Run(ctx, c, fmt.Sprintf("conn-%d", conns.Add(1)), "test-key", Deps{
			Registry:      reg,
			SigningSecret: "test-signing-secret",
			Fanout:        fan,
			RateLimit:     rl,
			Policy:        PolicyDisconnect{},
			Hub:           h,
		})
	}))
	srv.Config.ConnContext = WithNetConn
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http", "ws", 1)
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
	// A 1 s pong wait, so only a pong a full second late (not a scheduler
	// stall on a loaded -race runner) can end the conn.
	setKeepalive(t, 50*time.Millisecond, time.Second)
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

// TestKeepaliveIdleReaderOutlivesOldCutoff is the G2 regression at the
// production timers. The original bug was a fixed 60 s per-Read deadline,
// unrelated to pingInterval or pongWait, so the shortened-timer test above
// cannot see it come back. A healthy idle client must still be connected
// 65 s in. That takes 65 s, so -short skips it.
func TestKeepaliveIdleReaderOutlivesOldCutoff(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for 65 s at the production keepalive timers")
	}
	wsURL, ended := serveRun(t)
	start := time.Now()
	c := dialWS(t, wsURL)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	select {
	case at := <-ended:
		t.Fatalf("healthy idle client was disconnected after %v", at.Sub(start).Round(time.Second))
	case <-time.After(65 * time.Second):
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

// rawSubscribe reads the hello on a raw client, subscribes it to channel and
// returns once the ack is in. A raw client answers pings by hand; any that
// arrive here are answered at once.
func rawSubscribe(t *testing.T, nc net.Conn, br *bufio.Reader, channel string) {
	t.Helper()
	if _, _, err := readRawFrame(br); err != nil { // connected hello
		t.Fatalf("hello: %v", err)
	}
	if err := writeRawFrame(nc, 0x1, []byte(`{"type":"subscribe","channel":"`+channel+`"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		op, p, err := readRawFrame(br)
		if err != nil {
			t.Fatalf("subscribe ack: %v", err)
		}
		switch op {
		case 0x9:
			if err := writeRawFrame(nc, 0xA, p); err != nil {
				t.Fatal(err)
			}
		case 0x1:
			if !bytes.Contains(p, []byte(`"type":"subscribed"`)) {
				t.Fatalf("subscribe ack: got %s", p)
			}
			return
		}
	}
}

// TestSendQueueDrainsWhilePongPending is the regression for writePump waiting
// for the pong inside its select loop. Ping returns only once the pong is
// in, so for a whole ping round trip nothing drained the send queue. A
// subscriber that read every frame it was sent, but took 150 ms to answer a
// ping, was closed as a slow consumer when more events than the queue held
// were published in that window. The subscriber here holds its pong back
// until every event has arrived, so the events only get through if the queue
// drains while the pong is pending.
func TestSendQueueDrainsWhilePongPending(t *testing.T) {
	// The pong wait is far longer than the pong is held back, so the
	// liveness check cannot be what ends the conn.
	setKeepalive(t, 600*time.Millisecond, 5*time.Second)
	wsURL := serveSharedRun(t)

	// Raw, because a websocket.Conn answers pings by itself.
	nc, br := rawDial(t, wsURL)
	rawSubscribe(t, nc, br, "public-pong")

	pub := dialWS(t, wsURL)
	if _, _, err := pub.Read(context.Background()); err != nil { // connected hello
		t.Fatalf("publisher hello: %v", err)
	}
	sendJSON(t, pub, map[string]any{"type": "subscribe", "channel": "public-pong"})
	if got := readJSON(t, pub); got["type"] != "subscribed" {
		t.Fatalf("publisher ack: %+v", got)
	}

	// Take the subscriber's next ping and leave it unanswered.
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ping []byte
	for ping == nil {
		op, p, err := readRawFrame(br)
		if err != nil {
			t.Fatalf("waiting for a ping: %v", err)
		}
		if op == 0x9 {
			ping = p
		}
	}

	// More events than the 64 frames the queue held when this failed. The
	// publisher waits for its own copy of each event before the next
	// publish, so this is a steady stream rather than a burst: a healthy
	// subscriber's queue never holds more than a frame or two of it.
	const events = 90
	for i := 0; i < events; i++ {
		sendJSON(t, pub, map[string]any{"type": "publish", "channel": "public-pong", "data": i})
		if got := readJSON(t, pub); got["type"] != "event" {
			t.Fatalf("publisher's copy of event %d: %+v", i, got)
		}
	}

	_ = nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	for got := 0; got < events; {
		op, p, err := readRawFrame(br)
		if err != nil {
			t.Fatalf("got %d of %d events while the pong was pending: %v", got, events, err)
		}
		switch op {
		case 0x1:
			if bytes.Contains(p, []byte(`"type":"event"`)) {
				got++
			}
		case 0x8:
			code := 0
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p))
			}
			t.Fatalf("closed with %d after %d of %d events, with the pong not yet overdue", code, got, events)
		}
	}

	// A pong that is late but inside the pong wait keeps the conn up: the
	// next ping arrives.
	if err := writeRawFrame(nc, 0xA, ping); err != nil {
		t.Fatal(err)
	}
	for {
		op, _, err := readRawFrame(br)
		if err != nil {
			t.Fatalf("waiting for the ping after the late pong: %v", err)
		}
		if op == 0x9 {
			return
		}
	}
}

// TestPingGoroutineEndsWithConn extends the goroutine-leak invariant of
// internal/server/leak_test.go to the goroutine writePump runs each ping in.
// That test cannot reach it: it cannot shorten the keepalive timers from
// outside this package, and its conns close long before the first 30 s ping.
// Here every conn has a ping waiting for a pong when its client goes away,
// and the pong wait is far longer than the test, so a ping goroutine that
// outlived its conn would still be parked when the count is taken.
func TestPingGoroutineEndsWithConn(t *testing.T) {
	setKeepalive(t, 20*time.Millisecond, time.Minute)
	wsURL, ended := serveRun(t)

	// Warm up so the server's own goroutines are part of the baseline.
	_ = dialWS(t, wsURL).CloseNow()
	<-ended
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 50
	clients := make([]*websocket.Conn, n)
	for i := range clients {
		clients[i] = dialWS(t, wsURL) // never reads, so never answers a ping
	}
	time.Sleep(200 * time.Millisecond) // ten ping intervals: every conn has a ping in flight
	for _, c := range clients {
		_ = c.CloseNow()
	}
	for i := 0; i < n; i++ {
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d conns ended after their clients closed", i, n)
		}
	}

	const tolerance = 10
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+tolerance {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak suspected: base=%d after=%d (tolerance %d; one leaked ping goroutine per conn would be %d)",
		base, runtime.NumGoroutine(), tolerance, n)
}
