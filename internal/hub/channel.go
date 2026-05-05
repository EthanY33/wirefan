package hub

import "github.com/EthanY33/wirefan/internal/registry"

func Subscribe(c *registry.Channel, s registry.Subscriber) {
	c.SubsMu.Lock()
	c.Subscribers[s] = struct{}{}
	c.SubsMu.Unlock()
}

func Unsubscribe(c *registry.Channel, s registry.Subscriber) {
	c.SubsMu.Lock()
	delete(c.Subscribers, s)
	c.SubsMu.Unlock()
}

// Broadcast iterates subscribers under per-channel mutex (FIFO ordering).
func Broadcast(c *registry.Channel, msg []byte) {
	c.BroadcastMu.Lock()
	defer c.BroadcastMu.Unlock()
	c.SubsMu.RLock()
	subs := make([]registry.Subscriber, 0, len(c.Subscribers))
	for s := range c.Subscribers {
		subs = append(subs, s)
	}
	c.SubsMu.RUnlock()
	for _, s := range subs {
		_ = s.Send(msg) // policy resolution lives at the conn layer
	}
}

func SubscriberCount(c *registry.Channel) int {
	c.SubsMu.RLock()
	defer c.SubsMu.RUnlock()
	return len(c.Subscribers)
}
