package conn

// Policy is the slow-consumer backpressure strategy applied by Conn.Send when
// the per-conn send buffer is non-empty. Implementations decide whether to
// block, drop, or signal a disconnect. The buffer is full when sendQueue.offer
// refuses a frame: on its frame bound or on its byte bound.
type Policy interface {
	Apply(send *sendQueue, msg []byte, onSent func()) error
}

// PolicyDisconnect returns ErrSlowConsumer when the send buffer is full,
// signaling that the conn should be closed with code 1008.
type PolicyDisconnect struct{}

func (PolicyDisconnect) Apply(send *sendQueue, msg []byte, onSent func()) error {
	if !send.offer(msg) {
		return ErrSlowConsumer
	}
	if onSent != nil {
		onSent()
	}
	return nil
}

// PolicyDropOldest evicts the oldest queued message and enqueues the new one
// when the send buffer is full. The buffer can still be full after the
// eviction: sendAck and sendError write to it without taking Conn.sendMu and
// may take the freed slot, and on the byte bound the evicted message may be
// smaller than the new one. The new message is then dropped.
// Never blocks, never returns an error.
type PolicyDropOldest struct{}

func (PolicyDropOldest) Apply(send *sendQueue, msg []byte, onSent func()) error {
	if send.offer(msg) {
		return nil
	}
	send.evict()
	// Non-blocking retry: Conn.Send holds sendMu here, and a blocking send
	// would stall every broadcast to this conn behind a stuck writePump, or
	// forever once writePump has exited.
	send.offer(msg)
	return nil
}

// PolicyDropNewest discards the new message when the send buffer is full.
// Never returns an error.
type PolicyDropNewest struct{}

func (PolicyDropNewest) Apply(send *sendQueue, msg []byte, onSent func()) error {
	send.offer(msg) // dropped when refused
	return nil
}
