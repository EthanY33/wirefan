package conn

import (
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

func validateChannelName(name string) error {
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

// authRequiredPrefixes are channel name prefixes that require a signed token
// on subscribe. The token is verified against the channel name (and socket
// id), so a token for `private-alpha` cannot subscribe `private-bravo`.
var authRequiredPrefixes = []string{"private-", "presence-"}

func channelReserved(name string) bool {
	return strings.HasPrefix(name, reservedChannelPrefix)
}

func channelRequiresAuth(name string) bool {
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
		if err := validateChannelName(msg.Channel); err != nil {
			c.sendError("BAD_CHANNEL", err.Error())
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
	if channelReserved(msg.Channel) {
		c.sendError("RESERVED_CHANNEL", "channel name reserved for server use")
		return
	}
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendError("RATE_LIMITED", "too many control ops")
		return
	}
	if channelRequiresAuth(msg.Channel) {
		if err := auth.VerifyTokenAgainst(c.signingSecret, c.socketID, msg.Channel, msg.Token, c.replayCache); err != nil {
			metrics.AuthFails.Inc()
			if errors.Is(err, auth.ErrTokenReplayed) {
				c.sendError("AUTH_REPLAYED", "token already used")
				return
			}
			c.sendError("AUTH_FAILED", "invalid token")
			return
		}
	}
	c.subsMu.Lock()
	if _, already := c.subs[msg.Channel]; already {
		c.subsMu.Unlock()
		c.sendAck("subscribed", msg.Channel)
		return
	}
	if len(c.subs) >= c.maxChannels {
		c.subsMu.Unlock()
		c.sendError("LIMIT_CHANNELS", "max channels per conn")
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
			c.sendError("LIMIT_SUBSCRIBERS", "max subscribers per channel")
			return
		}
		c.sendError("SUBSCRIBE_FAILED", subErr.Error())
		return
	}
	c.subs[msg.Channel] = ch
	c.subsMu.Unlock()
	c.sendAck("subscribed", msg.Channel)
}

func (c *Conn) handlePublish(ctx context.Context, msg incoming) {
	if channelReserved(msg.Channel) {
		c.sendError("RESERVED_CHANNEL", "channel name reserved for server use")
		return
	}
	c.subsMu.Lock()
	ch, ok := c.subs[msg.Channel]
	c.subsMu.Unlock()
	if !ok {
		c.sendError("NOT_SUBSCRIBED", "must subscribe before publish")
		return
	}
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendError("RATE_LIMITED", "too many publishes for this API key")
		return
	}
	if !c.connRate.Allow() {
		c.sendError("RATE_LIMITED_CONN", "too many publishes on this connection")
		return
	}
	id := ulid.Make().String()
	out, _ := json.Marshal(map[string]any{
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

func (c *Conn) handleUnsubscribe(msg incoming) {
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendError("RATE_LIMITED", "too many control ops")
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

func (c *Conn) sendAck(typ, channel string) {
	b, _ := json.Marshal(map[string]string{"type": typ, "channel": channel})
	select {
	case c.send <- b:
	default:
	}
}

func (c *Conn) sendError(code, message string) {
	b, _ := json.Marshal(map[string]string{"type": "error", "code": code, "message": message})
	select {
	case c.send <- b:
	default:
	}
}

// Send satisfies the registry.Subscriber interface. Delegates to the configured
// backpressure Policy. On ErrSlowConsumer, signals Run to close the conn with
// 1008 (PolicyViolation).
//
// sendMu is required: PolicyDropOldest performs a non-atomic (default-send /
// drain / send) sequence on c.send. Two concurrent Send calls without this
// lock can both hit the drain branch, drain one message each, then both block
// on the unbuffered second send — head-of-line stalls under multi-channel
// fanout. PolicyDisconnect tolerates concurrency on its own (the send /
// default pair is atomic per goroutine), but the lock also costs nothing
// in the common path so we hold it unconditionally.
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
