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
// when the send buffer is full. Never returns an error.
type PolicyDropOldest struct{}

func (PolicyDropOldest) Apply(send chan []byte, msg []byte, onSent func()) error {
	select {
	case send <- msg:
	default:
		select {
		case <-send:
		default:
		}
		send <- msg
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
