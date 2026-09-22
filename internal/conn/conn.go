package conn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/coder/websocket"
	"golang.org/x/time/rate"
)

const (
	sendChanSize    = 64
	writeDeadline   = 10 * time.Second
	protocolVersion = "v1"
)

// Keepalive timers. writePump pings every pingInterval and ends the conn when
// the pong is not back within pongWait, so a dead or silent peer is dropped
// within about pingInterval + pongWait. There is deliberately no read
// deadline: coder/websocket handles pongs inside Read, so a pong never
// resets a per-Read timeout, and one would disconnect a healthy idle client
// that answers every ping. Vars rather than consts so tests can shorten them.
var (
	pingInterval = 30 * time.Second
	pongWait     = 10 * time.Second
)

// closeHandshakeTimeout bounds a server-side close handshake (CloseFrame).
// coder/websocket gives writing the close frame and reading the reply 5 s
// each, but not discarding the rest of a frame the peer stopped sending
// half way: a peer that stalls mid-frame holds that Close, and the socket,
// open for good. A Close still running after this long can only be stuck
// there, so CloseFrame then closes the TCP connection under it. A var so
// tests can shorten it.
var closeHandshakeTimeout = 15 * time.Second

// ErrSlowConsumer is returned by Conn.Send when the send buffer is full.
// Task 15's backpressure policy hooks here.
var ErrSlowConsumer = errors.New("slow consumer")

type Conn struct {
	ws            *websocket.Conn
	netConn       net.Conn // TCP conn under ws, or nil; see WithNetConn
	closeTimeout  time.Duration
	socketID      string
	apiKeyID      string
	send          chan []byte
	sendMu        sync.Mutex // serializes Send calls; required for PolicyDropOldest correctness (see Send)
	registry      registry.Registry
	signingSecret string
	replayCache   *auth.ReplayCache
	fanout        fanout.Fanout
	rateLimit     *ratelimit.Limiter // per-API-key bucket; shared across all conns owned by the key
	connRate      *rate.Limiter      // per-conn publish bucket; charged before rateLimit
	controlRate   *rate.Limiter      // per-conn subscribe/unsubscribe bucket; charged before rateLimit
	policy        Policy
	closeReq      chan struct{}
	cancel        context.CancelFunc // cancels Run's runCtx; see CloseNow
	subs          map[string]*registry.Channel
	subsMu        sync.Mutex
	closed        atomic.Bool
	maxChannels   int
}

// Spec'd resource limits. Hardcoded until flag wiring lands.
const (
	defaultMaxChannelsPerConn = 64
	defaultMaxSubsPerChannel  = 10000

	// Per-conn publish rate. The per-API-key bucket is shared across all
	// conns owned by a key; this layer bounds what a single socket can push,
	// independently of how many other conns the key has open. 50/s with a
	// burst of 100 is generous for legitimate UI clients (a chat app sees
	// far less) and well below what's needed to amplify a 64KB message into
	// a meaningful broadcast DoS at 10k subscribers.
	defaultConnPublishRate  = 50
	defaultConnPublishBurst = 100

	// Per-conn subscribe/unsubscribe rate. Control ops also draw from the
	// shared per-API-key bucket, so without this layer one socket spamming
	// junk unsubscribes could drain the key's budget and lock every other
	// client on the key out. The burst equals the per-conn channel cap so a
	// client re-joining a full channel set after a reconnect never trips
	// it; 20/s sustained is far above what a UI changes its subscriptions
	// at and well below the per-key refill, so one socket cannot empty it.
	defaultConnControlRate  = 20
	defaultConnControlBurst = defaultMaxChannelsPerConn
)

// APIKeyID implements the hub tracked-conn interface: Hub.CloseKey matches
// conns to a revoked key by it, and Hub.Add refuses a conn by it once its key
// has been revoked.
func (c *Conn) APIKeyID() string { return c.apiKeyID }

// CloseFrame implements the hub tracked-conn interface: Hub.Drain uses it to
// send shutdown closes to all tracked conns, and Hub.CloseKey to close the
// conns of a revoked key. It runs the close handshake and can block for
// several seconds on a peer that never answers. If the handshake is still
// running after closeHandshakeTimeout it closes the TCP connection, which
// ends the handshake.
func (c *Conn) CloseFrame(code websocket.StatusCode, reason string) {
	t := time.AfterFunc(c.closeTimeout, c.closeNetConn)
	defer t.Stop()
	_ = c.ws.Close(code, reason)
}

// CloseNow implements the hub tracked-conn interface: Hub.Drain force-closes
// conns still open when its deadline passes. It cancels runCtx, closes the
// TCP connection and returns at once; Run then finishes its normal teardown.
// Cancelling alone does not always free the socket: coder/websocket closes
// it only under a Read that is waiting on the network, and a CloseFrame
// handshake stuck discarding a half-sent frame owns the read side instead.
// ws.CloseNow would not do either: while a Close handshake is running it
// waits for that handshake rather than interrupting it.
func (c *Conn) CloseNow() {
	c.cancel()
	c.closeNetConn()
}

// closeNetConn closes the TCP connection under ws, when Run was given one.
// coder/websocket then fails whatever read or write it has in flight.
func (c *Conn) closeNetConn() {
	if c.netConn != nil {
		_ = c.netConn.Close()
	}
}

// netConnKey is the context key WithNetConn stores the TCP connection under.
type netConnKey struct{}

// WithNetConn returns ctx carrying nc, the TCP connection a request arrived
// on. It has the shape of http.Server.ConnContext, where server.New installs
// it: Run finds the connection under the upgraded WebSocket in its ctx and
// closes it when coder/websocket cannot (see CloseFrame and CloseNow).
// Without it Run still works, but a peer that stalls mid-frame during a
// server-side close handshake keeps its socket open.
func WithNetConn(ctx context.Context, nc net.Conn) context.Context {
	return context.WithValue(ctx, netConnKey{}, nc)
}

// Deps bundles the long-lived dependencies a Conn needs. All fields except
// ReplayCache are required; ReplayCache may be nil to disable subscribe-token
// replay protection (used by tests — production callers pass a process-wide
// cache so a leaked subscribe token cannot be reused within its 5-minute
// window).
type Deps struct {
	Registry      registry.Registry
	SigningSecret string
	ReplayCache   *auth.ReplayCache
	Fanout        fanout.Fanout
	RateLimit     *ratelimit.Limiter
	Policy        Policy
	Hub           *hub.Hub
}

// Run owns the conn for its lifetime. Returns when ctx is canceled or peer disconnects.
func Run(ctx context.Context, ws *websocket.Conn, socketID, apiKeyID string, d Deps) error {
	netConn, _ := ctx.Value(netConnKey{}).(net.Conn)
	c := &Conn{
		ws:            ws,
		netConn:       netConn,
		closeTimeout:  closeHandshakeTimeout,
		socketID:      socketID,
		apiKeyID:      apiKeyID,
		send:          make(chan []byte, sendChanSize),
		registry:      d.Registry,
		signingSecret: d.SigningSecret,
		replayCache:   d.ReplayCache,
		fanout:        d.Fanout,
		rateLimit:     d.RateLimit,
		connRate:      rate.NewLimiter(rate.Limit(defaultConnPublishRate), defaultConnPublishBurst),
		controlRate:   rate.NewLimiter(rate.Limit(defaultConnControlRate), defaultConnControlBurst),
		policy:        d.Policy,
		closeReq:      make(chan struct{}, 1),
		subs:          map[string]*registry.Channel{},
		maxChannels:   defaultMaxChannelsPerConn,
	}

	// runCtx exists before Hub.Add so a Drain that force-closes this conn
	// the moment it is tracked always has a cancel to call.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.cancel = cancel

	metrics.Connections.Inc()
	defer metrics.Connections.Dec()

	if ce, ok := d.Hub.Add(c); !ok {
		// Hub.CloseKey ran for this conn's key after the upgrade looked the
		// key up, too late for its sweep to see this conn. Close it the way
		// the sweep would have, before it can do anything.
		c.CloseFrame(ce.Code, ce.Reason)
		return ce
	}
	defer d.Hub.Remove(c)

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
		// Close the ws with 1008 BEFORE cancelling runCtx. If we cancel
		// first, writePump's in-flight c.ws.Write fails mid-frame and
		// coder/websocket tears down the conn with an abnormal-closure
		// code, never delivering the explicit PolicyViolation. Closing
		// first lets the close handshake serialize cleanly with the
		// pumps, and the subsequent cancel just unblocks them so they
		// return their (now-stale) errors.
		c.CloseFrame(websocket.StatusPolicyViolation, "slow consumer")
		cancel()
		<-errc
		<-errc
	}

	// Both pumps have returned, so tear the socket down on every exit path.
	// The oversize-message (1009) path and other pump errors used to return
	// here with the TCP connection still open and nobody reading it. Any
	// close frame is already out by now (written by the library on 1009 and
	// protocol errors, by the peer's handshake, or by the 1008 branch
	// above), so CloseNow only releases the socket, and it is a no-op once
	// a Close has finished. Not Close: its handshake discards the rest of a
	// half-read oversize frame with no deadline, so a peer that claims a
	// huge frame and then stalls would pin this goroutine forever. While a
	// CloseFrame handshake is still running, CloseNow waits for it instead,
	// up to 15 s; closing the TCP connection afterwards means the socket is
	// released whatever state the library was left in.
	_ = ws.CloseNow()
	c.closeNetConn()

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
