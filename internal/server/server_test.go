package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/store"
	"github.com/coder/websocket"
)

// newServedServer builds a Server with New and serves its public and admin
// http.Servers on loopback listeners, so a test exercises the timeouts and
// handlers New configured rather than an httptest copy. prepare runs after
// New and before the first Serve. It returns the store's one key id and the
// two listener addresses.
func newServedServer(t *testing.T, prepare func(s *Server)) (keyID, publicAddr, adminAddr string) {
	t.Helper()
	st := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, err := st.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	s := New(Config{AdminAddr: "127.0.0.1:0", AllowedOrigins: []string{"*"}}, Deps{
		Store:         st,
		AdminToken:    "admin-tok",
		Registry:      registry.NewSyncMap(),
		SigningSecret: "test-secret",
		Fanout:        fanout.NewPerConn(),
		RateLimit:     rl,
		Policy:        conn.PolicyDisconnect{},
		Hub:           hub.New(),
	})
	if prepare != nil {
		prepare(s)
	}
	serve := func(srv *http.Server) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		return ln.Addr().String()
	}
	return k.ID, serve(s.srv), serve(s.adminSrv)
}

// shortenReadTimeout scales a listener's production ReadTimeout down so a
// test does not wait the full value. A listener built without one is left
// alone: it has to fail the test that called this, not be handed the very
// timeout under test.
func shortenReadTimeout(srv *http.Server, d time.Duration) {
	if srv.ReadTimeout > 0 {
		srv.ReadTimeout = d
	}
}

// TestStalledRequestBodyIsReaped: a request whose headers are complete but
// whose declared body never arrives must not hold its connection open. With
// no ReadTimeout net/http clears the read deadline once the headers are
// parsed, so such a connection stayed open, unanswered, for as long as the
// peer kept the socket: past ReadHeaderTimeout and IdleTimeout, and outside
// the per-IP cap, which only counts accepted WebSockets. One host could pin
// file descriptors and goroutines without sending another byte. Every route
// on both listeners is covered, whether its handler reads the body (the
// /v1/keys decode) or returns without touching it (net/http then drains the
// body before it writes the response).
func TestStalledRequestBodyIsReaped(t *testing.T) {
	const (
		readTimeout = 300 * time.Millisecond
		wait        = 5 * time.Second
	)
	_, publicAddr, adminAddr := newServedServer(t, func(s *Server) {
		shortenReadTimeout(s.srv, readTimeout)
		shortenReadTimeout(s.adminSrv, readTimeout)
	})

	cases := []struct {
		name, addr, path, extraHeader string
	}{
		{"public health", publicAddr, "/v1/health", ""},
		{"public sign without credentials", publicAddr, "/v1/auth/sign", ""},
		{"public static root", publicAddr, "/", ""},
		{"public connect without key", publicAddr, "/v1/connect", ""},
		{"admin create key", adminAddr, "/v1/keys", "Authorization: Bearer admin-tok\r\n"},
		{"admin metrics", adminAddr, "/metrics", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := net.Dial("tcp", tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = nc.Close() }()
			// Complete headers, a declared body, and then nothing.
			req := "POST " + tc.path + " HTTP/1.1\r\nHost: x\r\n" + tc.extraHeader +
				"Content-Length: 1000\r\n\r\n"
			if _, err := io.WriteString(nc, req); err != nil {
				t.Fatal(err)
			}
			_ = nc.SetReadDeadline(time.Now().Add(wait))
			start := time.Now()
			// The server may answer first; either way it has to close the
			// connection, so reading to EOF must finish before the deadline.
			_, err = io.Copy(io.Discard, nc)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("connection with an unsent request body was still open %v after its headers", wait)
			}
			t.Logf("closed by the server after %v", time.Since(start).Round(10*time.Millisecond))
		})
	}
}

// TestReadTimeoutLeavesIdleWebSocketOpen guards the other side of the body
// read deadline: it must not cut WebSockets. A connection that upgrades
// through the server New builds stays silent for several ReadTimeouts and
// must still get its subscribe acked. net/http clears the read deadline
// before the handler of a request with no body runs, and Hijack clears every
// deadline on the connection, so ReadTimeout ends at the upgrade.
func TestReadTimeoutLeavesIdleWebSocketOpen(t *testing.T) {
	const readTimeout = 300 * time.Millisecond
	keyID, publicAddr, _ := newServedServer(t, func(s *Server) {
		s.srv.ReadTimeout = readTimeout
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+publicAddr+"/v1/connect?key="+keyID, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	if _, _, err := c.Read(ctx); err != nil { // connected hello
		t.Fatalf("hello: %v", err)
	}

	time.Sleep(4 * readTimeout)

	b, _ := json.Marshal(map[string]string{"type": "subscribe", "channel": "public-idle"})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("subscribe after idling %v: %v", 4*readTimeout, err)
	}
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("WebSocket was closed after idling %v: %v", 4*readTimeout, err)
	}
	if !bytes.Contains(raw, []byte(`"subscribed"`)) {
		t.Fatalf("want subscribed ack, got %s", raw)
	}
}
