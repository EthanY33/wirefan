package conn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
)

const (
	sendChanSize    = 64
	pingInterval    = 30 * time.Second
	readDeadline    = 60 * time.Second
	writeDeadline   = 10 * time.Second
	protocolVersion = "v1"
)

// ErrSlowConsumer is returned by Conn.Send when the send buffer is full.
// Task 15's backpressure policy hooks here.
var ErrSlowConsumer = errors.New("slow consumer")

type Conn struct {
	ws            *websocket.Conn
	socketID      string
	apiKeyID      string
	send          chan []byte
	registry      registry.Registry
	signingSecret string
	fanout        fanout.Fanout
	rateLimit     *ratelimit.Limiter
	policy        Policy
	closeReq      chan struct{}
	subs          map[string]*registry.Channel
	subsMu        sync.Mutex
	closed        atomic.Bool
	maxChannels   int
}

// Spec'd resource limits. Hardcoded until flag wiring lands.
const (
	defaultMaxChannelsPerConn   = 64
	defaultMaxSubsPerChannel    = 10000
)

// CloseFrame implements the hub.closer interface — used by Hub.Drain to broadcast
// shutdown closes to all tracked conns.
func (c *Conn) CloseFrame(code websocket.StatusCode, reason string) {
	_ = c.ws.Close(code, reason)
}

// Run owns the conn for its lifetime. Returns when ctx is canceled or peer disconnects.
func Run(ctx context.Context, ws *websocket.Conn, socketID, apiKeyID string, reg registry.Registry, signingSecret string, fan fanout.Fanout, rl *ratelimit.Limiter, pol Policy, h *hub.Hub) error {
	c := &Conn{
		ws:            ws,
		socketID:      socketID,
		apiKeyID:      apiKeyID,
		send:          make(chan []byte, sendChanSize),
		registry:      reg,
		signingSecret: signingSecret,
		fanout:        fan,
		rateLimit:     rl,
		policy:        pol,
		closeReq:      make(chan struct{}, 1),
		subs:          map[string]*registry.Channel{},
		maxChannels:   defaultMaxChannelsPerConn,
	}

	h.Add(c)
	defer h.Remove(c)

	hello, _ := json.Marshal(map[string]string{
		"type":      "connected",
		"socket_id": socketID,
		"version":   protocolVersion,
	})
	select {
	case c.send <- hello:
	default:
		return ws.Close(websocket.StatusInternalError, "send chan full at start")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, 2)
	go func() { errc <- c.writePump(runCtx) }()
	go func() { errc <- c.readPump(runCtx) }()

	var err error
	select {
	case err = <-errc:
		cancel()
		<-errc // drain the other pump
	case <-c.closeReq:
		err = ErrSlowConsumer
		cancel()
		// Neither pump has reported yet; drain both.
		<-errc
		<-errc
	}

	// On a slow-consumer disconnect, close the underlying ws with a
	// PolicyViolation code so the peer sees 1008. Pump-driven exits already
	// closed the ws (NormalClosure / GoingAway) inside their own returns.
	if errors.Is(err, ErrSlowConsumer) {
		_ = ws.Close(websocket.StatusPolicyViolation, "slow consumer")
	}

	if err != nil {
		slog.Debug("conn closed", "socket_id", socketID, "err", err)
	}

	// Mark closed BEFORE cleaning up subs, so any in-flight Broadcast snapshots
	// that still reference this conn will get ErrSlowConsumer from Send rather
	// than silently enqueueing into a dead chan.
	c.closed.Store(true)

	// Cleanup: unsubscribe from all channels on exit so registry doesn't leak
	// references to a dead conn. We intentionally do NOT delete empty channels
	// from the registry here — see handleUnsubscribe for the TOCTOU rationale.
	c.subsMu.Lock()
	subs := c.subs
	c.subs = nil
	c.subsMu.Unlock()
	for _, ch := range subs {
		hub.Unsubscribe(ch, c)
	}

	return err
}
