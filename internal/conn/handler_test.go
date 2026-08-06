package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestLimitChannelsPerConn(t *testing.T) {
	ws, _ := newTestConn(t, "test-secret")
	for i := 0; i < 64; i++ {
		sendJSON(t, ws, map[string]any{"type": "subscribe", "channel": fmt.Sprintf("public-%d", i)})
		if got := readJSON(t, ws); got["type"] != "subscribed" {
			t.Fatalf("subscribe %d: %+v", i, got)
		}
	}
	// 65th — expect LIMIT_CHANNELS error
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = Run(ctx, c, sid, "test-key", Deps{
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
	// c.send fills to sendChanSize and the next Send returns
	// ErrSlowConsumer.
	bigPayload := strings.Repeat("x", 50_000)
	for i := 0; i < 90; i++ {
		sendJSON(t, pub, map[string]any{
			"type":    "publish",
			"channel": "public-slow",
			"data":    map[string]any{"i": i, "blob": bigPayload},
		})
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
