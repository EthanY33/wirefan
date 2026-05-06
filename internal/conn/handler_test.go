package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		_ = Run(ctx, c, socketID, "test-key", registry.NewSyncMap(), signingSecret, fanout.NewPerConn(), rl, PolicyDisconnect{}, hub.New())
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
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
