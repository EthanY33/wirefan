package conn

import "sync/atomic"

// sendQueue is a conn's outbound frame queue: a buffered channel, which keeps
// the frames in FIFO order and lets writePump select on it, plus a count of
// the bytes queued, so the queue is bounded in bytes as well as in frames.
// The bounds are sendChanSize, sendUnmeteredFrames and sendQueueBytes
// (conn.go).
type sendQueue struct {
	ch    chan []byte
	bytes atomic.Int64 // total length of the frames in ch
}

func newSendQueue() *sendQueue {
	return &sendQueue{ch: make(chan []byte, sendChanSize)}
}

// offer queues msg unless that would pass a bound, and reports whether it
// did. It never blocks. The queue refuses a frame when it already holds
// sendChanSize frames, or when it holds sendUnmeteredFrames or more and msg
// would take it past sendQueueBytes.
//
// The check and the enqueue are not one atomic step. Conn.Send serializes
// its callers with sendMu, but sendAck and sendErrorFrame offer without it,
// so one ack or error frame (under 1 KiB) can land on top of a bound.
func (q *sendQueue) offer(msg []byte) bool {
	n := int64(len(msg))
	if len(q.ch) >= sendUnmeteredFrames && q.bytes.Load()+n > sendQueueBytes {
		return false
	}
	// Count the bytes before the frame is visible to writePump, so the
	// count never runs below what is queued.
	q.bytes.Add(n)
	select {
	case q.ch <- msg:
		return true
	default:
		q.bytes.Add(-n)
		return false
	}
}

// evict drops the oldest queued frame, if there is one, and reports whether
// it did. It never blocks.
func (q *sendQueue) evict() bool {
	select {
	case msg := <-q.ch:
		q.release(msg)
		return true
	default:
		return false
	}
}

// release takes msg's bytes off the count. writePump calls it for every
// frame it receives from ch.
func (q *sendQueue) release(msg []byte) {
	q.bytes.Add(-int64(len(msg)))
}
