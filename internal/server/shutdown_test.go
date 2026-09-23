package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestDrainClosesAllConnections(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()

	upgrader := NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	})
	srv := httptest.NewServer(upgrader)
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	// Open 5 conns; each starts a Run goroutine on the server side
	conns := make([]*websocket.Conn, 0, 5)
	var clientWG sync.WaitGroup
	for i := 0; i < 5; i++ {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Read the connected hello so the server has fully attached
		_, _, _ = c.Read(context.Background())
		conns = append(conns, c)
		clientWG.Add(1)
		go func(c *websocket.Conn) {
			defer clientWG.Done()
			// Read until close
			for {
				if _, _, err := c.Read(context.Background()); err != nil {
					return
				}
			}
		}(c)
	}

	// Allow registration to settle
	time.Sleep(100 * time.Millisecond)

	// Drain
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.Drain(drainCtx, 5*time.Second)
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return within 10s")
	}

	// All client readers should have observed close
	clientDone := make(chan struct{})
	go func() {
		clientWG.Wait()
		close(clientDone)
	}()
	select {
	case <-clientDone:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("client conns did not close within 2s of Drain")
	}

	for _, c := range conns {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
}

// TestDrainNonReadingPeersHonorsCtx is the G3 end-to-end check. A peer that
// never reads never answers the close handshake, so each server-side Close
// sits in coder/websocket's 5 s handshake wait. Drain used to run those one
// at a time under the hub lock, ignoring ctx: five such peers held shutdown
// for about 25 s. Drain must return within its ctx and leave no conn open.
func TestDrainNonReadingPeersHonorsCtx(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()
	srv := httptest.NewServer(NewUpgradeHandler(UpgradeDeps{
		Store:          s,
		AllowedOrigins: []string{"*"},
		Registry:       registry.NewSyncMap(),
		SigningSecret:  "test-secret",
		Fanout:         fanout.NewPerConn(),
		RateLimit:      rl,
		Policy:         conn.PolicyDisconnect{},
		Hub:            h,
	}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

	const peers = 5
	for i := 0; i < peers; i++ {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		// Read only the hello, then go silent for good.
		if _, _, err := c.Read(context.Background()); err != nil {
			t.Fatalf("hello %d: %v", i, err)
		}
	}
	waitForLen(t, h, peers)

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	h.Drain(drainCtx, 30*time.Second)
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("Drain took %v with a 1 s ctx", elapsed)
	}
	waitForLen(t, h, 0)
}

// waitForLen polls until h tracks exactly n conns, failing after 2 s.
func waitForLen(t *testing.T, h *hub.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.Len() != n {
		if time.Now().After(deadline) {
			t.Fatalf("hub tracks %d conns, want %d", h.Len(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDrainFreesSocketOfPeerStalledMidFrame checks, through the http.Server
// that New builds, that conn.Run is handed the TCP connection
// (conn.WithNetConn). A peer that stalls mid-frame while the server's close
// handshake reads from it leaves coder/websocket unable to close the socket,
// and only that connection lets Drain's force close release it. The conn
// package tests cover the mechanism; this covers the wiring.
func TestDrainFreesSocketOfPeerStalledMidFrame(t *testing.T) {
	st := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := st.CreateKey(context.Background(), "t", auth.HashSecret(secret))
	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	h := hub.New()
	s := New(Config{AllowedOrigins: []string{"*"}}, Deps{
		Store:         st,
		AdminToken:    "admin-tok",
		Registry:      registry.NewSyncMap(),
		SigningSecret: "test-secret",
		Fanout:        fanout.NewPerConn(),
		RateLimit:     rl,
		Policy:        conn.PolicyDisconnect{},
		Hub:           h,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() { _ = s.srv.Close() })

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	req := "GET /v1/connect?key=" + k.ID + " HTTP/1.1\r\nHost: " + ln.Addr().String() +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", res, err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	// readOp returns the opcode of the next server frame (unmasked, and
	// every frame here is under 126 bytes).
	readOp := func() byte {
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if _, err := io.CopyN(io.Discard, br, int64(hdr[1]&0x7f)); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		return hdr[0] & 0x0f
	}
	readOp() // connected hello
	waitForLen(t, h, 1)

	// Same choreography as conn's stallMidFrame: half a fragment, close
	// frame, finish the fragment, half of the next one, then silence.
	const fragLen = 1024
	half := bytes.Repeat([]byte("x"), fragLen/2)
	first := append([]byte{0x01, 0x80 | 126, fragLen >> 8, fragLen & 0xff, 0, 0, 0, 0}, half...)
	cont := append([]byte{0x80, 0x80 | 126, fragLen >> 8, fragLen & 0xff, 0, 0, 0, 0}, half...)
	if _, err := nc.Write(first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	drained := make(chan struct{})
	go func() {
		h.Drain(context.Background(), time.Second)
		close(drained)
	}()
	for readOp() != 0x8 {
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := nc.Write(half); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := nc.Write(cont); err != nil {
		t.Fatal(err)
	}

	_ = nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = io.Copy(io.Discard, br)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("socket still open 3 s after the peer stalled mid-frame")
	}
	<-drained
}

// TestRunShutdownIsBoundedAndNotFatal: the listeners get their own shutdown
// budget, and running out of it is not an error. Before, Drain and both
// listener Shutdowns shared one 30 s context, so a slow peer that used up
// the drain window left srv.Shutdown an expired context; any HTTP request
// still in flight then made Run return context.DeadlineExceeded, which main
// logs as fatal and exits 1 on, so systemd recorded every such stop as a
// failure. Here a request that never finishes its headers stays active past
// the listener budget: Run must still return nil, and promptly.
func TestRunShutdownIsBoundedAndNotFatal(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	rl := ratelimit.New(100, 200, time.Hour)
	t.Cleanup(rl.Close)
	s := New(Config{Addr: addr, AllowedOrigins: []string{"*"}}, Deps{
		Store:         store.NewMemory(),
		AdminToken:    "admin-tok",
		Registry:      registry.NewSyncMap(),
		SigningSecret: "test-secret",
		Fanout:        fanout.NewPerConn(),
		RateLimit:     rl,
		Policy:        conn.PolicyDisconnect{},
		Hub:           hub.New(),
	})
	s.drainGrace = 200 * time.Millisecond
	s.listenerGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var nc net.Conn
	for deadline := time.Now().Add(3 * time.Second); ; {
		if nc, err = net.Dial("tcp", addr); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never listened: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = nc.Close() })
	// Half a request: the conn is active, and ReadHeaderTimeout (10 s) is
	// far longer than the listener budget.
	if _, err := io.WriteString(nc, "GET /v1/health HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on a normal shutdown; want nil", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("Run took %v to shut down; the budgets are 200 ms each", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 s of cancel")
	}
}
