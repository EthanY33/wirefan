package conn

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
)

func TestConnectedMessageSent(t *testing.T) {
	var got map[string]any
	var gotMu sync.Mutex

	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)

	handler := func(c *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = Run(ctx, c, "01HTEST", "test-key", Deps{
			Registry:      registry.NewSyncMap(),
			SigningSecret: "test-signing-secret",
			Fanout:        fanout.NewPerConn(),
			RateLimit:     rl,
			Policy:        PolicyDisconnect{},
			Hub:           hub.New(),
		})
	}

	srv := httptest.NewServer(websocketHandler(handler))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	_, data, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gotMu.Lock()
	_ = json.Unmarshal(data, &got)
	gotMu.Unlock()

	if got["type"] != "connected" || got["socket_id"] != "01HTEST" {
		t.Fatalf("got %+v", got)
	}
}

// websocketHandler accepts a WS upgrade and passes the connected websocket.Conn to fn.
// Used to test conn-level behavior with a real upgraded conn rather than mocking.
func websocketHandler(fn func(*websocket.Conn)) http.Handler {
	return websocketHandlerCtx(func(_ context.Context, c *websocket.Conn) { fn(c) })
}

// websocketHandlerCtx is websocketHandler that also passes the request
// context, as the upgrade handler does to Run.
func websocketHandlerCtx(fn func(context.Context, *websocket.Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		fn(r.Context(), c)
	})
}

// rawDial performs the WebSocket opening handshake by hand so a test can see
// what the server does at the TCP level, which a websocket.Conn client hides
// (it answers a close frame and tears down its own side).
func rawDial(t *testing.T, wsURL string) (net.Conn, *bufio.Reader) {
	t.Helper()
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := "GET / HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: want 101, got %d", res.StatusCode)
	}
	return nc, br
}

// writeRawFrame writes one client frame. Clients must mask; an all-zero
// masking key leaves the payload bytes unchanged.
func writeRawFrame(w io.Writer, opcode byte, payload []byte) error {
	hdr := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	hdr = append(hdr, 0, 0, 0, 0)
	_, err := w.Write(append(hdr, payload...))
	return err
}

// readRawFrame reads one unmasked server frame.
func readRawFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0] & 0x0f, p, nil
}

// TestOversizeMessageClosesSocket is the DB3 regression. On the 1009 path the
// library writes the close frame, readPump returns, and Run used to return
// without closing the socket, so the TCP connection lingered with nobody
// reading it. The client must see the connection end promptly.
func TestOversizeMessageClosesSocket(t *testing.T) {
	wsURL, ended := serveRun(t)
	nc, br := rawDial(t, wsURL)
	if _, _, err := readRawFrame(br); err != nil { // connected hello
		t.Fatalf("hello: %v", err)
	}

	// Over the 64 KiB read limit.
	if err := writeRawFrame(nc, 0x1, bytes.Repeat([]byte("x"), 70_000)); err != nil {
		t.Fatal(err)
	}

	_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	sawClose := false
	for {
		op, p, err := readRawFrame(br)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("socket still open 2 s after the oversize message (saw close frame: %v)", sawClose)
			}
			break // EOF or reset: the server tore the connection down
		}
		if op == 0x8 && !sawClose {
			sawClose = true
			if len(p) >= 2 {
				if code := binary.BigEndian.Uint16(p); code != uint16(websocket.StatusMessageTooBig) {
					t.Fatalf("close code: want 1009, got %d", code)
				}
				// Answer like a well-behaved peer would. The server may
				// already have released the socket, so a failed write
				// is fine; the read below decides.
				_ = writeRawFrame(nc, 0x8, p[:2])
			}
		}
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the socket closed")
	}
}

// TestRunRefusesKeyClosedBeforeAdd is the G6 residual-race regression. An
// upgrade can look its key up just before a revoke and reach Hub.Add just
// after Hub.CloseKey took its snapshot, so the revoke never closed it and the
// conn lived on. Run must close such a conn with the close CloseKey sent,
// before the hello, and never leave it tracked.
func TestRunRefusesKeyClosedBeforeAdd(t *testing.T) {
	h := hub.New()
	if n := h.CloseKey("test-key", websocket.StatusPolicyViolation, "key revoked"); n != 0 {
		t.Fatalf("CloseKey closed %d conns before any dial", n)
	}
	wsURL, ended := serveRunOn(t, h)
	c := dialWS(t, wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("conn on a closed key: want a close frame, got err %v, frame %s", err, raw)
	}
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "key revoked" {
		t.Fatalf("want 1008 %q, got %d %q", "key revoked", ce.Code, ce.Reason)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after refusing the conn")
	}
	if n := h.Len(); n != 0 {
		t.Fatalf("hub still tracks %d conns", n)
	}
}

// stallMidFrame drives a raw client into the one state coder/websocket cannot
// leave on its own. The client streams a fragmented message; trigger starts
// a server-side close handshake while readPump holds the read lock mid-frame.
// Once the close frame arrives the client finishes that fragment, which hands
// the read lock to the handshake, then sends half of one more fragment and
// goes silent. The handshake now discards the rest of that fragment one byte
// at a time with no deadline. Returns once the client has stalled.
func stallMidFrame(t *testing.T, nc net.Conn, br *bufio.Reader, trigger func()) {
	t.Helper()
	const fragLen = 1024
	half := bytes.Repeat([]byte("x"), fragLen/2)
	// Masked, zero masking key, 16-bit length: FIN clear on the first
	// fragment, set on the continuation.
	first := append([]byte{0x01, 0x80 | 126, fragLen >> 8, fragLen & 0xff, 0, 0, 0, 0}, half...)
	cont := append([]byte{0x80, 0x80 | 126, fragLen >> 8, fragLen & 0xff, 0, 0, 0, 0}, half...)

	if _, _, err := readRawFrame(br); err != nil { // connected hello
		t.Fatalf("hello: %v", err)
	}
	if _, err := nc.Write(first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // readPump now waits for the rest

	trigger()
	_ = nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		op, _, err := readRawFrame(br)
		if err != nil {
			t.Fatalf("waiting for the close frame: %v", err)
		}
		if op == 0x8 {
			break
		}
	}
	time.Sleep(100 * time.Millisecond) // the handshake now waits for the read lock
	if _, err := nc.Write(half); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the handshake has the lock and reads the next header
	if _, err := nc.Write(cont); err != nil {
		t.Fatal(err)
	}
}

// waitSocketClosed fails unless the server ends the TCP connection within d.
func waitSocketClosed(t *testing.T, nc net.Conn, br *bufio.Reader, d time.Duration) {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, br)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("socket still open %v after the peer stalled mid-frame", d)
	}
}

// TestCloseHandshakeStalledMidFrame is the regression for a peer that stalls
// mid-frame during a server-side close handshake. coder/websocket then blocks
// discarding the unfinished frame with no deadline, and neither ws.CloseNow
// nor cancelling Run's context closes the socket, so the handshake goroutine
// and the TCP connection used to outlive the conn for good. Both hub close
// paths must still release the socket: CloseKey once the handshake overruns
// closeHandshakeTimeout, Drain as soon as its grace runs out.
func TestCloseHandshakeStalledMidFrame(t *testing.T) {
	t.Run("CloseKey", func(t *testing.T) {
		setCloseHandshakeTimeout(t, time.Second)
		h := hub.New()
		wsURL, ended := serveRunOn(t, h)
		nc, br := rawDial(t, wsURL)
		stallMidFrame(t, nc, br, func() {
			if n := h.CloseKey("test-key", websocket.StatusPolicyViolation, "key revoked"); n != 1 {
				t.Fatalf("CloseKey closed %d conns, want 1", n)
			}
		})
		waitSocketClosed(t, nc, br, 3*time.Second)
		select {
		case <-ended:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after the socket closed")
		}
	})
	t.Run("Drain", func(t *testing.T) {
		h := hub.New()
		wsURL, ended := serveRunOn(t, h)
		nc, br := rawDial(t, wsURL)
		drained := make(chan struct{})
		stallMidFrame(t, nc, br, func() {
			go func() {
				h.Drain(context.Background(), time.Second)
				close(drained)
			}()
		})
		waitSocketClosed(t, nc, br, 3*time.Second)
		select {
		case <-ended:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after the socket closed")
		}
		<-drained
	})
}
