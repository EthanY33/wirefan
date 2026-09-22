package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/oklog/ulid/v2"
)

// Cap on channel-name byte length. Combined with the per-conn maxChannels
// cap, this bounds how much registry memory a single connection can pin.
// Without it, an authenticated peer can spam subscribes with random
// 1 MiB names; the registry never garbage-collects, so memory grows
// monotonically with attacker effort.
const maxChannelNameLen = 128

// ValidateChannelName enforces the channel-name rules shared by the WS
// protocol and POST /v1/auth/sign: non-empty, at most maxChannelNameLen
// bytes, no control characters.
func ValidateChannelName(name string) error {
	if name == "" {
		return errors.New("channel name is empty")
	}
	if len(name) > maxChannelNameLen {
		return fmt.Errorf("channel name exceeds %d bytes", maxChannelNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("channel name contains control character")
		}
	}
	return nil
}

type incoming struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel"`
	Token   string          `json:"token,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Channel-name ACLs.
//
// The original handler treated anything not starting with `private-` as
// public, no auth required, and rejected `_`-prefixed names only on publish.
// That's two gaps:
//
//  1. `presence-` is a Pusher-protocol convention for authenticated
//     channels with member metadata; without explicit handling, an attacker
//     could subscribe to `presence-room-1` unauthenticated and read every
//     publish to it.
//
//  2. Subscribe ignored `_`-prefixed names, so an attacker could grab a
//     reserved channel before the server ever uses it. Reject on both
//     subscribe and publish.
//
// The split is data-driven so future prefixes are a one-liner.

const reservedChannelPrefix = "_"

// statsChannel is the one reserved channel clients may subscribe to,
// read-only. The server's hub.PublishStatsLoop broadcasts metric snapshots
// on it and the bundled demo page renders them as live stat tiles. Client
// publish to it is still rejected (handlePublish checks channelReserved
// before anything else), and every other "_"-prefixed name stays fully
// reserved on both subscribe and publish. The snapshot payload contains
// only aggregate counters (connections, channels, published, dropped), so
// exposing it read-only leaks no per-tenant or per-channel data.
const statsChannel = "_wirefan-stats"

// reservedSubscribeAllowed reports whether a client may subscribe to a reserved
// channel name. Only the stats channel is exempt from the reservation.
func reservedSubscribeAllowed(name string) bool {
	return name == statsChannel
}

// authRequiredPrefixes are channel name prefixes that require a signed token
// on subscribe. The token is verified against the channel name (and socket
// id), so a token for `private-alpha` cannot subscribe `private-bravo`.
var authRequiredPrefixes = []string{"private-", "presence-"}

func channelReserved(name string) bool {
	return strings.HasPrefix(name, reservedChannelPrefix)
}

// ChannelRequiresAuth reports whether subscribing to name needs a signed
// token (a private- or presence- channel). POST /v1/auth/sign refuses to
// mint tokens for any other channel.
func ChannelRequiresAuth(name string) bool {
	for _, p := range authRequiredPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (c *Conn) handle(ctx context.Context, raw []byte) {
	var msg incoming
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.sendError("BAD_JSON", "malformed message")
		return
	}
	switch msg.Type {
	case "subscribe", "unsubscribe", "publish":
		if err := ValidateChannelName(msg.Channel); err != nil {
			c.sendOpError(msg, "BAD_CHANNEL", err.Error())
			return
		}
	}
	switch msg.Type {
	case "subscribe":
		c.handleSubscribe(msg)
	case "unsubscribe":
		c.handleUnsubscribe(msg)
	case "publish":
		c.handlePublish(ctx, msg)
	default:
		c.sendError("BAD_TYPE", "unknown message type")
	}
}

// maxSubscribeRetries bounds how many times handleSubscribe re-attempts
// GetOrCreate when the registry GC races with the subscribe. Three is more
// than enough — Sweep runs at the minute scale, the retry window is the
// time between two registry calls, so >1 retry would be a hot bug.
const maxSubscribeRetries = 3

func (c *Conn) handleSubscribe(msg incoming) {
	if channelReserved(msg.Channel) && !reservedSubscribeAllowed(msg.Channel) {
		c.sendOpError(msg, "RESERVED_CHANNEL", "channel name reserved for server use")
		return
	}
	if !c.allowControl(msg) {
		return
	}
	// A channel this conn already holds is acked before the token check: a
	// re-subscribe grants nothing new, so demanding a token (and burning it
	// in the replay cache) would only break clients that repeat a subscribe.
	// handle runs solely on readPump, so the channel cannot be added to
	// c.subs between this check and the insert below.
	c.subsMu.Lock()
	_, already := c.subs[msg.Channel]
	c.subsMu.Unlock()
	if already {
		c.sendAck("subscribed", msg.Channel)
		return
	}
	if ChannelRequiresAuth(msg.Channel) {
		if err := auth.VerifyTokenAgainst(c.signingSecret, c.socketID, msg.Channel, msg.Token, c.replayCache); err != nil {
			metrics.AuthFails.Inc()
			if errors.Is(err, auth.ErrTokenReplayed) {
				c.sendOpError(msg, "AUTH_REPLAYED", "token already used")
				return
			}
			c.sendOpError(msg, "AUTH_FAILED", "invalid token")
			return
		}
	}
	c.subsMu.Lock()
	if len(c.subs) >= c.maxChannels {
		c.subsMu.Unlock()
		c.sendOpError(msg, "LIMIT_CHANNELS", "max channels per conn")
		return
	}
	var ch *registry.Channel
	var subErr error
	for i := 0; i < maxSubscribeRetries; i++ {
		ch = c.registry.GetOrCreate(msg.Channel)
		subErr = hub.Subscribe(ch, c, defaultMaxSubsPerChannel)
		if subErr == nil {
			break
		}
		if errors.Is(subErr, hub.ErrChannelDeleted) {
			continue
		}
		break
	}
	if subErr != nil {
		c.subsMu.Unlock()
		if errors.Is(subErr, hub.ErrTooManySubs) {
			c.sendOpError(msg, "LIMIT_SUBSCRIBERS", "max subscribers per channel")
			return
		}
		c.sendOpError(msg, "SUBSCRIBE_FAILED", subErr.Error())
		return
	}
	c.subs[msg.Channel] = ch
	c.subsMu.Unlock()
	c.sendAck("subscribed", msg.Channel)
}

func (c *Conn) handlePublish(ctx context.Context, msg incoming) {
	if channelReserved(msg.Channel) {
		c.sendOpError(msg, "RESERVED_CHANNEL", "channel name reserved for server use")
		return
	}
	c.subsMu.Lock()
	ch, ok := c.subs[msg.Channel]
	c.subsMu.Unlock()
	if !ok {
		c.sendOpError(msg, "NOT_SUBSCRIBED", "must subscribe before publish")
		return
	}
	// Per-conn bucket first: a publish this socket's own limit rejects must
	// not also spend the per-key budget every other conn on the key shares.
	if !c.connRate.Allow() {
		c.sendOpError(msg, "RATE_LIMITED_CONN", "too many publishes on this connection")
		return
	}
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendOpError(msg, "RATE_LIMITED", "too many publishes for this API key")
		return
	}
	id := ulid.Make().String()
	out := marshalFrame(map[string]any{
		"type":    "event",
		"channel": msg.Channel,
		"data":    msg.Data,
		"id":      id,
	})
	start := time.Now()
	metrics.Published.Inc()
	c.fanout.Broadcast(ctx, ch, out)
	metrics.Latency.Observe(time.Since(start).Seconds())
}

// allowControl charges a subscribe or unsubscribe to the per-conn control
// bucket and then the shared per-key bucket, answering msg with the matching
// error when either is empty. The per-conn check comes first so one socket
// spamming control frames exhausts only its own budget, never the key's.
func (c *Conn) allowControl(msg incoming) bool {
	if !c.controlRate.Allow() {
		c.sendOpError(msg, "RATE_LIMITED_CONN", "too many control ops on this connection")
		return false
	}
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendOpError(msg, "RATE_LIMITED", "too many control ops")
		return false
	}
	return true
}

func (c *Conn) handleUnsubscribe(msg incoming) {
	if !c.allowControl(msg) {
		return
	}
	c.subsMu.Lock()
	ch, ok := c.subs[msg.Channel]
	if !ok {
		c.subsMu.Unlock()
		c.sendAck("unsubscribed", msg.Channel)
		return
	}
	delete(c.subs, msg.Channel)
	c.subsMu.Unlock()
	hub.Unsubscribe(ch, c)
	// Empty channels are GC'd asynchronously by registry.SweepLoop. Subscribers
	// arriving after Sweep marks a channel Deleted retry GetOrCreate, which
	// returns a fresh non-deleted channel.
	c.sendAck("unsubscribed", msg.Channel)
}

// marshalFrame encodes an outbound frame without HTML escaping. By default
// encoding/json rewrites '<', '>' and '&' as six-byte \u00XX sequences, and it
// does so inside json.RawMessage too, so a relayed publish payload could grow
// up to 6x before being copied to every subscriber. Frames go to WebSocket
// clients, never into an HTML document, so the escaping buys nothing.
func marshalFrame(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (c *Conn) sendAck(typ, channel string) {
	b := marshalFrame(map[string]string{"type": typ, "channel": channel})
	select {
	case c.send <- b:
	default:
	}
}

// errorFrame is the wire shape of every error. Op and Channel are set only
// when the error answers a subscribe, unsubscribe or publish frame: Op is
// that frame's type and Channel the channel exactly as the client sent it,
// so a client can match the error to the request that caused it. Both are
// omitted for BAD_JSON and BAD_TYPE, and Channel is omitted when the frame
// had none or its channel is longer than maxChannelNameLen (see
// sendOpError). Clients ignore unknown fields, so this is additive within v1.
type errorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Op      string `json:"op,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// sendError sends an error that answers no particular request (BAD_JSON,
// BAD_TYPE).
func (c *Conn) sendError(code, message string) {
	c.sendErrorFrame(errorFrame{Type: "error", Code: code, Message: message})
}

// sendOpError sends an error answering msg, a subscribe, unsubscribe or
// publish frame. A channel longer than maxChannelNameLen is not echoed. Such
// a name cannot be a real channel, and echoing it let one 64 KiB frame queue
// a 64 KiB error with no rate limit, 64 deep per conn (up to 384 KiB before
// marshalFrame stopped HTML-escaping). With the cap an error frame stays
// under 1 KiB.
func (c *Conn) sendOpError(msg incoming, code, message string) {
	f := errorFrame{Type: "error", Code: code, Message: message, Op: msg.Type}
	if len(msg.Channel) <= maxChannelNameLen {
		f.Channel = msg.Channel
	}
	c.sendErrorFrame(f)
}

func (c *Conn) sendErrorFrame(f errorFrame) {
	b := marshalFrame(f)
	select {
	case c.send <- b:
	default:
	}
}

// Send satisfies the registry.Subscriber interface. Delegates to the configured
// backpressure Policy. On ErrSlowConsumer, signals Run to close the conn with
// 1008 (PolicyViolation).
//
// sendMu serializes PolicyDropOldest's non-atomic (try-send / evict /
// try-send) sequence on c.send, so concurrent multi-channel broadcasts each
// evict at most the one message their own insert needs. Every step of that
// sequence is non-blocking, so holding the lock can never stall a
// broadcaster behind a stuck or exited writePump. PolicyDisconnect tolerates
// concurrency on its own (the send / default pair is atomic per goroutine),
// but the lock also costs nothing in the common path so we hold it
// unconditionally.
func (c *Conn) Send(b []byte) error {
	if c.closed.Load() {
		return ErrSlowConsumer
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	err := c.policy.Apply(c.send, b, nil)
	if errors.Is(err, ErrSlowConsumer) {
		metrics.Dropped.WithLabelValues("slow_consumer").Inc()
		// Non-blocking: if a previous Send already signaled, skip.
		select {
		case c.closeReq <- struct{}{}:
		default:
		}
	}
	return err
}

// Close satisfies the registry.Subscriber interface. Intentionally a no-op:
// the Run loop owns the connection lifecycle and closing c.send here would
// race with writePump.
func (c *Conn) Close() {}
