package conn

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/oklog/ulid/v2"
)

type incoming struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel"`
	Token   string          `json:"token,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (c *Conn) handle(raw []byte) {
	var msg incoming
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.sendError("BAD_JSON", "malformed message")
		return
	}
	switch msg.Type {
	case "subscribe":
		c.handleSubscribe(msg)
	case "unsubscribe":
		c.handleUnsubscribe(msg)
	case "publish":
		c.handlePublish(msg)
	default:
		c.sendError("BAD_TYPE", "unknown message type")
	}
}

func (c *Conn) handleSubscribe(msg incoming) {
	if strings.HasPrefix(msg.Channel, "private-") {
		if err := auth.VerifyToken(c.signingSecret, c.socketID, msg.Channel, msg.Token); err != nil {
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
	ch := c.registry.GetOrCreate(msg.Channel)
	hub.Subscribe(ch, c)
	c.subs[msg.Channel] = ch
	c.subsMu.Unlock()
	c.sendAck("subscribed", msg.Channel)
}

func (c *Conn) handlePublish(msg incoming) {
	c.subsMu.Lock()
	ch, ok := c.subs[msg.Channel]
	c.subsMu.Unlock()
	if !ok {
		c.sendError("NOT_SUBSCRIBED", "must subscribe before publish")
		return
	}
	if !c.rateLimit.Allow(c.apiKeyID) {
		c.sendError("RATE_LIMITED", "too many publishes")
		return
	}
	id := ulid.Make().String()
	out, _ := json.Marshal(map[string]any{
		"type":    "event",
		"channel": msg.Channel,
		"data":    msg.Data,
		"id":      id,
	})
	c.fanout.Broadcast(context.Background(), ch, out)
}

func (c *Conn) handleUnsubscribe(msg incoming) {
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
	// NOTE: empty channels are intentionally NOT deleted from the registry here.
	// A cross-conn TOCTOU race with concurrent GetOrCreate could orphan a Channel
	// reference. Empty channels are kept and reused; a GC pass for genuinely
	// abandoned channels can land in a later task.
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

// Send satisfies the registry.Subscriber interface. Returns ErrSlowConsumer
// when the per-conn send buffer is full; Task 15's policy hooks here.
func (c *Conn) Send(b []byte) error {
	if c.closed.Load() {
		return ErrSlowConsumer
	}
	select {
	case c.send <- b:
		return nil
	default:
		return ErrSlowConsumer
	}
}

// Close satisfies the registry.Subscriber interface. Intentionally a no-op:
// the Run loop owns the connection lifecycle and closing c.send here would
// race with writePump.
func (c *Conn) Close() {}
