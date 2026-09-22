package conn

// Policy is the slow-consumer backpressure strategy applied by Conn.Send when
// the per-conn send buffer is non-empty. Implementations decide whether to
// block, drop, or signal a disconnect.
type Policy interface {
	Apply(send chan []byte, msg []byte, onSent func()) error
}

// PolicyDisconnect returns ErrSlowConsumer when the send buffer is full,
// signaling that the conn should be closed with code 1008.
type PolicyDisconnect struct{}

func (PolicyDisconnect) Apply(send chan []byte, msg []byte, onSent func()) error {
	select {
	case send <- msg:
		if onSent != nil {
			onSent()
		}
		return nil
	default:
		return ErrSlowConsumer
	}
}

// PolicyDropOldest evicts the oldest queued message and enqueues the new one
// when the send buffer is full. The buffer can still be full after the
// eviction, because sendAck and sendError write to it without taking
// Conn.sendMu and may take the freed slot; the new message is then dropped.
// Never blocks, never returns an error.
type PolicyDropOldest struct{}

func (PolicyDropOldest) Apply(send chan []byte, msg []byte, onSent func()) error {
	select {
	case send <- msg:
		return nil
	default:
	}
	select {
	case <-send:
	default:
	}
	// Non-blocking retry: Conn.Send holds sendMu here, and a blocking send
	// would stall every broadcast to this conn behind a stuck writePump, or
	// forever once writePump has exited.
	select {
	case send <- msg:
	default:
	}
	return nil
}

// PolicyDropNewest discards the new message when the send buffer is full.
// Never returns an error.
type PolicyDropNewest struct{}

func (PolicyDropNewest) Apply(send chan []byte, msg []byte, onSent func()) error {
	select {
	case send <- msg:
		return nil
	default:
		return nil // drop
	}
}
