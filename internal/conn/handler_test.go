package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
)

func newTestConn(t *testing.T, signingSecret string) (*websocket.Conn, string) {
	t.Helper()
	return newTestConnWithCache(t, signingSecret, nil)
}

// newTestConnWithCache is newTestConn with a subscribe-token replay cache
// (nil disables replay protection, as in newTestConn).
func newTestConnWithCache(t *testing.T, signingSecret string, cache *auth.ReplayCache) (*websocket.Conn, string) {
	t.Helper()
	const socketID = "01HTEST"
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = Run(ctx, c, socketID, "test-key", Deps{
			Registry:      registry.NewSyncMap(),
			SigningSecret: signingSecret,
			ReplayCache:   cache,
			Fanout:        fanout.NewPerConn(),
			RateLimit:     rl,
			Policy:        PolicyDisconnect{},
			Hub:           hub.New(),
		})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	// Drain the connected hello frame.
	if _, _, err := c.Read(context.Background()); err != nil {
		t.Fatalf("reading hello: %v", err)
	}
	return c, socketID
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_, raw, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func TestSubscribePublicNoToken(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "public-room"})
	got := readJSON(t, c)
	if got["type"] != "subscribed" || got["channel"] != "public-room" {
		t.Fatalf("got %+v", got)
	}
}

func TestSubscribePrivateValidHMAC(t *testing.T) {
	const secret = "test-signing-secret"
	c, socketID := newTestConn(t, secret)
	tok, err := auth.SignToken(secret, socketID, "private-x", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	sendJSON(t, c, map[string]any{
		"type":    "subscribe",
		"channel": "private-x",
		"token":   tok,
	})
	got := readJSON(t, c)
	if got["type"] != "subscribed" || got["channel"] != "private-x" {
		t.Fatalf("got %+v", got)
	}
}

func TestSubscribePrivateBadHMAC(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{
		"type":    "subscribe",
		"channel": "private-y",
		"token":   "garbage",
	})
	got := readJSON(t, c)
	if got["type"] != "error" || got["code"] != "AUTH_FAILED" {
		t.Fatalf("expected AUTH_FAILED error frame, got %+v", got)
	}

	// Verify connection is still alive: another Read should time out (no further
	// frame coming) rather than return a close error. The spec mandates that
	// bad-HMAC must NOT close the conn.
	rctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := c.Read(rctx)
	if err == nil {
		t.Fatalf("expected read timeout after bad-HMAC error frame, got frame")
	}
	// We expect a context deadline (timeout) — NOT a websocket close error.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded (conn still open), got %v", err)
	}
}

func TestDuplicateSubscribeIdempotent(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "public-dup"})
	first := readJSON(t, c)
	if first["type"] != "subscribed" || first["channel"] != "public-dup" {
		t.Fatalf("first ack: %+v", first)
	}
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "public-dup"})
	second := readJSON(t, c)
	if second["type"] != "subscribed" || second["channel"] != "public-dup" {
		t.Fatalf("second ack: %+v", second)
	}
}

// TestResubscribeHeldChannelNeedsNoToken is the DB6 regression. Re-subscribing
// to a channel this conn already holds is a no-op that grants nothing new, so
// it must ack without requiring a token and without consuming one. Before the
// fix the token check ran first: a tokenless re-subscribe got AUTH_FAILED and
// a fresh token was burned in the replay cache for nothing.
func TestResubscribeHeldChannelNeedsNoToken(t *testing.T) {
	const secret = "test-signing-secret"
	cache := auth.NewReplayCache()
	c, socketID := newTestConnWithCache(t, secret, cache)
	tok, err := auth.SignToken(secret, socketID, "private-x", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "private-x", "token": tok})
	if got := readJSON(t, c); got["type"] != "subscribed" {
		t.Fatalf("first subscribe: %+v", got)
	}

	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "private-x"})
	if got := readJSON(t, c); got["type"] != "subscribed" || got["channel"] != "private-x" {
		t.Fatalf("tokenless re-subscribe: want subscribed, got %+v", got)
	}

	fresh, err := auth.SignToken(secret, socketID, "private-x", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "private-x", "token": fresh})
	if got := readJSON(t, c); got["type"] != "subscribed" {
		t.Fatalf("re-subscribe with token: %+v", got)
	}
	if err := auth.VerifyTokenAgainst(secret, socketID, "private-x", fresh, cache); err != nil {
		t.Fatalf("re-subscribe consumed the token: %v", err)
	}
}

func TestLimitChannelsPerConn(t *testing.T) {
	ws, _ := newTestConn(t, "test-secret")
	for i := 0; i < 64; i++ {
		sendJSON(t, ws, map[string]any{"type": "subscribe", "channel": fmt.Sprintf("public-%d", i)})
		if got := readJSON(t, ws); got["type"] != "subscribed" {
			t.Fatalf("subscribe %d: %+v", i, got)
		}
	}
	// 65th: expect a LIMIT_CHANNELS error. The 64 subscribes spent the whole
	// per-conn control burst; wait for a refill so the channel cap, not the
	// rate limit, is what answers.
	time.Sleep(2 * time.Second / defaultConnControlRate)
	sendJSON(t, ws, map[string]any{"type": "subscribe", "channel": "public-overflow"})
	got := readJSON(t, ws)
	if got["type"] != "error" || got["code"] != "LIMIT_CHANNELS" {
		t.Fatalf("expected LIMIT_CHANNELS, got %+v", got)
	}
}

func TestSubscribePresenceRequiresAuth(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{
		"type":    "subscribe",
		"channel": "presence-room",
	})
	got := readJSON(t, c)
	if got["type"] != "error" || got["code"] != "AUTH_FAILED" {
		t.Fatalf("expected AUTH_FAILED for unauthed presence-room, got %+v", got)
	}
}

func TestSubscribePresenceValidHMAC(t *testing.T) {
	const secret = "test-signing-secret"
	c, socketID := newTestConn(t, secret)
	tok, err := auth.SignToken(secret, socketID, "presence-room", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	sendJSON(t, c, map[string]any{
		"type":    "subscribe",
		"channel": "presence-room",
		"token":   tok,
	})
	got := readJSON(t, c)
	if got["type"] != "subscribed" || got["channel"] != "presence-room" {
		t.Fatalf("got %+v", got)
	}
}

func TestSubscribeReservedRejected(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "_internal"})
	got := readJSON(t, c)
	if got["type"] != "error" || got["code"] != "RESERVED_CHANNEL" {
		t.Fatalf("expected RESERVED_CHANNEL, got %+v", got)
	}
}

// TestSubscribeStatsChannelAllowed proves the one carve-out from the
// reserved-channel rule: clients may subscribe (read-only) to exactly
// "_wirefan-stats" so the demo's live stat tiles work.
func TestSubscribeStatsChannelAllowed(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "_wirefan-stats"})
	got := readJSON(t, c)
	if got["type"] != "subscribed" || got["channel"] != "_wirefan-stats" {
		t.Fatalf("expected subscribed/_wirefan-stats, got %+v", got)
	}
}

// TestPublishStatsChannelRejected proves the carve-out is read-only: a
// client subscribed to "_wirefan-stats" still cannot publish to it.
func TestPublishStatsChannelRejected(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "_wirefan-stats"})
	if got := readJSON(t, c); got["type"] != "subscribed" {
		t.Fatalf("subscribe ack: %+v", got)
	}
	sendJSON(t, c, map[string]any{
		"type":    "publish",
		"channel": "_wirefan-stats",
		"data":    map[string]any{"forged": true},
	})
	got := readJSON(t, c)
	if got["type"] != "error" || got["code"] != "RESERVED_CHANNEL" {
		t.Fatalf("expected RESERVED_CHANNEL on publish, got %+v", got)
	}
}

// TestSubscribeOtherReservedStillRejected proves the carve-out is exact:
// every other "_"-prefixed name, including near-misses of the stats
// channel, remains reserved on subscribe.
func TestSubscribeOtherReservedStillRejected(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	for _, name := range []string{"_wirefan-stats2", "_wirefan", "_stats", "__wirefan-stats"} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "channel": name})
		got := readJSON(t, c)
		if got["type"] != "error" || got["code"] != "RESERVED_CHANNEL" {
			t.Fatalf("channel %q: expected RESERVED_CHANNEL, got %+v", name, got)
		}
	}
}

// newSharedEnvConns spins up an httptest WS server backed by ONE registry
// + hub + rate limiter so that publishes on one conn fan out to subscribers
// on the other. Each dialed conn gets a unique socket-id ("conn-0", "conn-1",
// ...) and the hello frame is drained before returning.
func newSharedEnvConns(t *testing.T, pol Policy, n int) []*websocket.Conn {
	t.Helper()
	reg := registry.NewSyncMap()
	h := hub.New()
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	fan := fanout.NewPerConn()
	// Server-side conns live until the test ends, not for a fixed time: a
	// slow or loaded runner must not cut a conn off mid-test.
	runCtx, cancelRuns := context.WithCancel(context.Background())
	t.Cleanup(cancelRuns)

	var idCounter int
	var idMu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		idMu.Lock()
		sid := fmt.Sprintf("conn-%d", idCounter)
		idCounter++
		idMu.Unlock()
		_ = Run(runCtx, c, sid, "test-key", Deps{
			Registry:      reg,
			SigningSecret: "test-signing-secret",
			Fanout:        fan,
			RateLimit:     rl,
			Policy:        pol,
			Hub:           h,
		})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	conns := make([]*websocket.Conn, n)
	for i := 0; i < n; i++ {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Drain hello.
		if _, _, err := c.Read(context.Background()); err != nil {
			t.Fatalf("conn %d hello: %v", i, err)
		}
		conns[i] = c
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
	})
	return conns
}

func TestPublishRoundTrip(t *testing.T) {
	conns := newSharedEnvConns(t, PolicyDisconnect{}, 2)
	pub, sub := conns[0], conns[1]

	// Both conns subscribe to a public channel. Order matters: sub must
	// be subscribed before pub publishes, otherwise the fanout has no
	// subscriber on the other side.
	sendJSON(t, sub, map[string]any{"type": "subscribe", "channel": "public-rt"})
	if got := readJSON(t, sub); got["type"] != "subscribed" {
		t.Fatalf("sub ack: %+v", got)
	}
	sendJSON(t, pub, map[string]any{"type": "subscribe", "channel": "public-rt"})
	if got := readJSON(t, pub); got["type"] != "subscribed" {
		t.Fatalf("pub ack: %+v", got)
	}

	sendJSON(t, pub, map[string]any{
		"type":    "publish",
		"channel": "public-rt",
		"data":    map[string]any{"hello": "world"},
	})

	// Sub should see the event envelope. Pub may also see it (it's
	// subscribed too), but we only assert on sub.
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := sub.Read(rctx)
	if err != nil {
		t.Fatalf("sub read: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("sub unmarshal %q: %v", raw, err)
	}
	if ev["type"] != "event" {
		t.Fatalf("type: want event, got %v", ev["type"])
	}
	if ev["channel"] != "public-rt" {
		t.Fatalf("channel: want public-rt, got %v", ev["channel"])
	}
	if _, ok := ev["id"].(string); !ok {
		t.Fatalf("id: want string, got %T %v", ev["id"], ev["id"])
	}
	data, ok := ev["data"].(map[string]any)
	if !ok || data["hello"] != "world" {
		t.Fatalf("data: want {hello:world}, got %v", ev["data"])
	}
}

func TestSlowConsumerDisconnects(t *testing.T) {
	conns := newSharedEnvConns(t, PolicyDisconnect{}, 2)
	pub, slow := conns[0], conns[1]

	sendJSON(t, slow, map[string]any{"type": "subscribe", "channel": "public-slow"})
	if got := readJSON(t, slow); got["type"] != "subscribed" {
		t.Fatalf("slow ack: %+v", got)
	}
	sendJSON(t, pub, map[string]any{"type": "subscribe", "channel": "public-slow"})
	if got := readJSON(t, pub); got["type"] != "subscribed" {
		t.Fatalf("pub ack: %+v", got)
	}

	// Slow conn now stops reading. To deterministically trigger
	// ErrSlowConsumer on loopback we need to backpressure TCP, not just
	// fill sendChanSize — loopback's TCP buffer is large enough that
	// small JSON envelopes drain through writePump as fast as Broadcast
	// can enqueue them, leaving c.send momentarily empty between Sends.
	// Use a payload close to the 64 KiB ReadLimit so a handful of frames
	// saturate the kernel's TCP send buffer (~200 KiB on Linux, similar
	// order of magnitude on Windows). With writePump blocked on Write,
	// c.send fills. Frames this size pass sendQueueBytes long before
	// sendChanSize, so the queue stops at sendUnmeteredFrames (64, where
	// it always did) and the next Send returns ErrSlowConsumer.
	bigPayload := strings.Repeat("x", 50_000)
	// pub is subscribed to the channel it publishes on, so it receives its
	// own echoes. Drain them so the publisher is not itself a stalled
	// subscriber; only slow is meant to stop reading.
	pub.SetReadLimit(1 << 20)
	go func() {
		for {
			if _, _, err := pub.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 90; i++ {
		b, err := json.Marshal(map[string]any{
			"type":    "publish",
			"channel": "public-slow",
			"data":    map[string]any{"i": i, "blob": bigPayload},
		})
		if err != nil {
			t.Fatal(err)
		}
		wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = pub.Write(wctx, websocket.MessageText, b)
		wcancel()
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// Read from slow until the server closes the conn. Raise the client
	// read limit above coder/websocket's 32 KiB default so the big
	// payloads queued in the TCP buffer before the slow-consumer signal
	// don't surface as "message too big" — we only care about the close.
	//
	// We assert the BEHAVIOR (slow consumer is disconnected within the
	// deadline), not the wire-format detail (close code 1008). The 1008
	// frame can only be delivered when ws.Close can acquire the writer
	// mutex — but on a fully-stuck slow consumer the writePump holds
	// that mutex inside a blocked TCP Write. The kernel's loopback TCP
	// buffer size determines whether writePump is stuck mid-frame or
	// between frames when the signal fires, and that buffer differs
	// across platforms (Windows ~1 MiB, Linux ~200 KiB). The protocol
	// contract is "best-effort 1008"; the disconnect itself is the
	// load-bearing guarantee.
	slow.SetReadLimit(1 << 20)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for slow-consumer close")
		}
		rctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, _, err := slow.Read(rctx)
		cancel()
		if err == nil {
			continue // got an event frame, keep waiting for the close
		}
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		// Any non-deadline error means the conn is closed — slow-consumer
		// disconnect successful. CloseStatus may return 1008 (clean) or -1
		// (abnormal); both indicate the server severed the conn.
		return
	}
}

// TestAdmittedBurstKeepsReadersConnected is the regression for a send queue
// smaller than the publish burst. The queue held 64 frames while a fresh conn
// may publish 100 at once, and readPump handles a burst that is already in
// its socket buffer without yielding: every event was queued for every
// subscriber before any writePump ran, the 65th Send overflowed, and conns
// that were reading the whole time were closed with 1008 as slow consumers.
// A burst the rate limits admit must reach every reader and close none.
func TestAdmittedBurstKeepsReadersConnected(t *testing.T) {
	// One P, as on a 1 vCPU host, so the publisher's readPump runs the
	// whole burst before a writePump gets a turn.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	const subscribers = 20
	const burst = defaultConnPublishBurst
	conns := newSharedEnvConns(t, PolicyDisconnect{}, subscribers+1)
	for i, c := range conns {
		sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "public-burst"})
		if got := readJSON(t, c); got["type"] != "subscribed" {
			t.Fatalf("conn %d ack: %+v", i, got)
		}
	}

	// Every conn, the publisher included, reads as fast as frames arrive.
	type result struct {
		events int
		err    error
	}
	results := make(chan result, len(conns))
	for _, c := range conns {
		go func(c *websocket.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := 0
			for n < burst {
				_, raw, err := c.Read(ctx)
				if err != nil {
					results <- result{n, err}
					return
				}
				if bytes.Contains(raw, []byte(`"type":"event"`)) {
					n++
				}
			}
			results <- result{n, nil}
		}(c)
	}

	// The whole per-conn publish burst, back to back.
	pub := conns[subscribers]
	frame := []byte(`{"type":"publish","channel":"public-burst","data":"xxxxxxxxxxxxxxxxxxxx"}`)
	for i := 0; i < burst; i++ {
		if err := pub.Write(context.Background(), websocket.MessageText, frame); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	lost := 0
	for range conns {
		if r := <-results; r.err != nil {
			lost++
			t.Logf("a reading conn got %d of %d events, then: %v", r.events, burst, r.err)
		}
	}
	if lost != 0 {
		t.Fatalf("%d of %d reading conns did not get a burst the rate limits admit", lost, len(conns))
	}
}

// TestAdmittedLargeFrameBurstKeepsReadersConnected is the regression for a
// burst the rate limits admit that totals more than sendQueueBytes. One conn
// writes its whole publish burst (100 frames of 16,000 bytes, about 1.5 MiB)
// in a single TCP write, so readPump has every frame in its socket buffer. It
// used to queue all of them for every subscriber before a writePump got a
// turn, the byte bound refused the tail, and subscribers that were reading
// the whole time were closed as slow consumers. readPump now yields after
// each admitted publish, so no reader may be closed.
func TestAdmittedLargeFrameBurstKeepsReadersConnected(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	const subscribers = 20
	const burst = defaultConnPublishBurst
	const payload = 16_000
	wsURL := serveSharedRun(t)

	type result struct {
		events int
		err    error
	}
	results := make(chan result, subscribers)
	for i := 0; i < subscribers; i++ {
		c := dialWS(t, wsURL)
		c.SetReadLimit(1 << 20)
		if _, _, err := c.Read(context.Background()); err != nil {
			t.Fatalf("hello: %v", err)
		}
		sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "public-big"})
		if got := readJSON(t, c); got["type"] != "subscribed" {
			t.Fatalf("ack: %+v", got)
		}
		go func(c *websocket.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			n := 0
			for n < burst {
				_, raw, err := c.Read(ctx)
				if err != nil {
					results <- result{n, err}
					return
				}
				// marshalFrame sorts keys, so "type" comes last.
				if bytes.HasSuffix(raw, []byte(`"type":"event"}`)) {
					n++
				}
			}
			results <- result{n, nil}
		}(c)
	}

	// The publisher is raw so the whole burst can go out in one write. It
	// is subscribed too (a publish needs that) and keeps reading its own
	// echoes and answering pings.
	nc, br := rawDial(t, wsURL)
	rawSubscribe(t, nc, br, "public-big")
	go func() {
		for {
			op, p, err := readRawFrame(br)
			if err != nil {
				return
			}
			if op == 0x9 {
				_ = writeRawFrame(nc, 0xA, p)
			}
		}
	}()
	frame := []byte(`{"type":"publish","channel":"public-big","data":"` + strings.Repeat("x", payload) + `"}`)
	var all bytes.Buffer
	for i := 0; i < burst; i++ {
		if err := writeRawFrame(&all, 0x1, frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nc.Write(all.Bytes()); err != nil {
		t.Fatalf("burst write: %v", err)
	}

	lost := 0
	for i := 0; i < subscribers; i++ {
		if r := <-results; r.err != nil {
			lost++
			t.Logf("a reading conn got %d of %d events, then: %v", r.events, burst, r.err)
		}
	}
	if lost != 0 {
		t.Fatalf("%d of %d reading conns were cut off by one conn's admitted burst of %d frames of %d bytes",
			lost, subscribers, burst, payload)
	}
}

// TestSpammingConnCannotStarveKey is the G4 regression. Every conn on an API
// key shares one per-key bucket, and the per-conn publish limit used to be
// charged only after it (control ops had no per-conn limit at all). One
// socket could therefore burn the whole key budget with junk unsubscribes,
// or with publishes its own per-conn limit then rejected, and lock every
// other client on the key out. Per-conn limits now come first, so a
// spammer hits RATE_LIMITED_CONN and the key keeps budget for others.
func TestSpammingConnCannotStarveKey(t *testing.T) {
	for _, op := range []string{"unsubscribe", "publish"} {
		t.Run(op, func(t *testing.T) {
			conns := newSharedEnvConns(t, PolicyDisconnect{}, 2)
			spam, victim := conns[0], conns[1]
			if op == "publish" {
				sendJSON(t, spam, map[string]any{"type": "subscribe", "channel": "public-spam"})
				if got := readJSON(t, spam); got["type"] != "subscribed" {
					t.Fatalf("spam subscribe: %+v", got)
				}
			}

			// Well past the per-key burst (200). One reply per frame: an
			// ack, the spammer's own event, or an error.
			codes := map[string]int{}
			for i := 0; i < 400; i++ {
				switch op {
				case "unsubscribe":
					sendJSON(t, spam, map[string]any{"type": "unsubscribe", "channel": "public-never-held"})
				case "publish":
					sendJSON(t, spam, map[string]any{"type": "publish", "channel": "public-spam", "data": i})
				}
				if got := readJSON(t, spam); got["type"] == "error" {
					codes[got["code"].(string)]++
				}
			}
			if codes["RATE_LIMITED_CONN"] == 0 {
				t.Fatalf("spammer never hit its per-conn limit: %v", codes)
			}
			if codes["RATE_LIMITED"] != 0 {
				t.Fatalf("spammer drained the shared per-key bucket: %v", codes)
			}

			sendJSON(t, victim, map[string]any{"type": "subscribe", "channel": "public-victim"})
			if got := readJSON(t, victim); got["type"] != "subscribed" {
				t.Fatalf("second conn on the key was starved: %+v", got)
			}
		})
	}
}

func TestUnsubscribeRoundTrip(t *testing.T) {
	ws, _ := newTestConn(t, "test-secret")
	sendJSON(t, ws, map[string]any{"type": "subscribe", "channel": "public-x"})
	if got := readJSON(t, ws); got["type"] != "subscribed" {
		t.Fatalf("expected subscribed, got %+v", got)
	}
	sendJSON(t, ws, map[string]any{"type": "unsubscribe", "channel": "public-x"})
	if got := readJSON(t, ws); got["type"] != "unsubscribed" || got["channel"] != "public-x" {
		t.Fatalf("expected unsubscribed/public-x, got %+v", got)
	}
	// Idempotent — unsubscribe again should still ack
	sendJSON(t, ws, map[string]any{"type": "unsubscribe", "channel": "public-x"})
	if got := readJSON(t, ws); got["type"] != "unsubscribed" {
		t.Fatalf("expected idempotent unsubscribed, got %+v", got)
	}
}

// wantError asserts an error frame with the given code, op and channel.
// An empty op or channel means the field must be absent.
func wantError(t *testing.T, got map[string]any, code, op, channel string) {
	t.Helper()
	if got["type"] != "error" || got["code"] != code {
		t.Fatalf("want error %s, got %+v", code, got)
	}
	if _, ok := got["message"].(string); !ok {
		t.Fatalf("error frame without message: %+v", got)
	}
	for _, f := range []struct{ key, want string }{{"op", op}, {"channel", channel}} {
		v, present := got[f.key]
		switch {
		case f.want == "" && present:
			t.Fatalf("%s: want no %q field, got %+v", code, f.key, got)
		case f.want != "" && v != f.want:
			t.Fatalf("%s: want %s %q, got %+v", code, f.key, f.want, got)
		}
	}
}

// TestErrorFramesCarryOpAndChannel covers the error-frame contract: errors
// answering a subscribe, unsubscribe or publish frame echo "op" and the
// channel exactly as sent; BAD_JSON and BAD_TYPE carry neither.
func TestErrorFramesCarryOpAndChannel(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")

	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "_internal"})
	wantError(t, readJSON(t, c), "RESERVED_CHANNEL", "subscribe", "_internal")

	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "private-y", "token": "garbage"})
	wantError(t, readJSON(t, c), "AUTH_FAILED", "subscribe", "private-y")

	sendJSON(t, c, map[string]any{"type": "unsubscribe", "channel": "bad\x01name"})
	wantError(t, readJSON(t, c), "BAD_CHANNEL", "unsubscribe", "bad\x01name")

	sendJSON(t, c, map[string]any{"type": "unsubscribe"})
	wantError(t, readJSON(t, c), "BAD_CHANNEL", "unsubscribe", "")

	sendJSON(t, c, map[string]any{"type": "publish", "channel": "public-z", "data": 1})
	wantError(t, readJSON(t, c), "NOT_SUBSCRIBED", "publish", "public-z")

	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"type":`)); err != nil {
		t.Fatal(err)
	}
	wantError(t, readJSON(t, c), "BAD_JSON", "", "")

	sendJSON(t, c, map[string]any{"type": "nope", "channel": "public-x"})
	wantError(t, readJSON(t, c), "BAD_TYPE", "", "")
}

// TestOverlongChannelNotEchoed bounds what an error frame can cost. Echoing
// the channel of a frame rejected for a too-long name let one 64 KiB frame
// queue an error of up to 384 KiB (json.Marshal writes '<' as \u003c), with
// no rate limit, 64 deep per conn. Such a name can never be a real channel,
// so the error carries op and code but not the channel, and stays small. A
// name at the limit is still echoed exactly.
func TestOverlongChannelNotEchoed(t *testing.T) {
	c, _ := newTestConn(t, "test-signing-secret")
	c.SetReadLimit(1 << 20) // so an oversized error frame fails the size check below, not the read

	// Written by hand: json.Marshal would escape the '<'s past the read limit.
	frame := `{"type":"subscribe","channel":"` + strings.Repeat("<", 60_000) + `"}`
	if err := c.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 {
		t.Fatalf("error frame for an overlong channel is %d bytes, want under 1 KiB", len(raw))
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	wantError(t, got, "BAD_CHANNEL", "subscribe", "")

	atLimit := "private-" + strings.Repeat("<", maxChannelNameLen-len("private-"))
	sendJSON(t, c, map[string]any{"type": "subscribe", "channel": atLimit, "token": "garbage"})
	wantError(t, readJSON(t, c), "AUTH_FAILED", "subscribe", atLimit)
}

// TestPublishDoesNotInflatePayload pins the event envelope to the size of
// what was published. json.Marshal HTML-escapes '<', '>' and '&' inside a
// json.RawMessage (each becomes a six-byte \u00XX), so a 60 KB publish of
// '<' used to reach every subscriber as a ~360 KB event: a 6x amplifier on
// top of the fanout itself.
func TestPublishDoesNotInflatePayload(t *testing.T) {
	conns := newSharedEnvConns(t, PolicyDisconnect{}, 2)
	pub, sub := conns[0], conns[1]
	sub.SetReadLimit(1 << 20) // room to observe an inflated event
	for _, c := range []*websocket.Conn{sub, pub} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "inflate"})
		if got := readJSON(t, c); got["type"] != "subscribed" {
			t.Fatalf("ack: %+v", got)
		}
	}

	payload := strings.Repeat("<&>", 20000) // 60,000 bytes of HTML-special chars
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	frame := `{"type":"publish","channel":"inflate","data":"` + payload + `"}`
	if err := pub.Write(wctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := sub.Read(rctx)
	if err != nil {
		t.Fatalf("sub read: %v", err)
	}
	if limit := len(payload) + 200; len(raw) > limit {
		t.Fatalf("event is %d bytes for a %d-byte payload (limit %d): payload was re-escaped", len(raw), len(payload), limit)
	}
	var ev struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Data != payload {
		t.Fatalf("data changed in transit (%d bytes in, %d out)", len(payload), len(ev.Data))
	}
}

// TestPublishRejectsInvalidUTF8: json.Unmarshal accepts any byte above 0x1f
// inside a JSON string and json.RawMessage keeps the raw bytes, so a publish
// carrying invalid UTF-8 used to be relayed inside a text frame. RFC 6455
// requires a browser to fail the connection on invalid UTF-8 in a text
// message, so one such publish disconnected every browser on the channel.
// The server must refuse the frame and relay nothing.
func TestPublishRejectsInvalidUTF8(t *testing.T) {
	conns := newSharedEnvConns(t, PolicyDisconnect{}, 2)
	pub, sub := conns[0], conns[1]
	for _, c := range []*websocket.Conn{sub, pub} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "channel": "utf8"})
		if got := readJSON(t, c); got["type"] != "subscribed" {
			t.Fatalf("ack: %+v", got)
		}
	}

	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	bad := []byte("{\"type\":\"publish\",\"channel\":\"utf8\",\"data\":\"\xff\xfe\"}")
	if err := pub.Write(wctx, websocket.MessageText, bad); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := readJSON(t, pub); got["type"] != "error" || got["code"] != "BAD_JSON" {
		t.Fatalf("publisher: want a BAD_JSON error, got %+v", got)
	}

	rctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, raw, err := sub.Read(rctx); err == nil {
		t.Fatalf("subscriber received a relayed frame: %q", raw)
	}
}

// TestSharedEnvConnOutlivesTenSeconds is the regression for the shared test
// server giving every Run a hard 10 s lifetime: a test that ran past it had
// its socket torn down under it and failed with a connection reset that said
// nothing about the code under test.
func TestSharedEnvConnOutlivesTenSeconds(t *testing.T) {
	t.Parallel()
	conns := newSharedEnvConns(t, PolicyDisconnect{}, 1)
	time.Sleep(10500 * time.Millisecond)
	sendJSON(t, conns[0], map[string]any{"type": "subscribe", "channel": "public-late"})
	if got := readJSON(t, conns[0]); got["type"] != "subscribed" {
		t.Fatalf("ack after 10.5 s: %+v", got)
	}
}
