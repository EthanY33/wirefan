package conn

import (
	"errors"
	"testing"
)

// keyBurst is the largest burst the rate limiters admit: the per-key bucket
// (ratelimit.New(100, 200, ...) in cmd/wirefan/main.go: run), which every
// conn on a key draws on. This package cannot import that value.
const keyBurst = 200

// maxFrame is the inbound message cap set in readPump, which is also about
// the size of the largest event a conn can be sent.
const maxFrame = 64 * 1024

// TestSendQueueBounds pins the three bounds of the per-conn send queue. The
// frame bound has to stay above the largest burst the rate limiters admit,
// or such a burst overflows the queue of a subscriber that is reading (at 64
// slots a 100-frame burst did). The byte bound keeps the larger frame bound
// from raising what one conn can hold in memory. The first 64 frames are
// taken whatever their size, so nothing the old 64-slot queue accepted is
// refused.
func TestSendQueueBounds(t *testing.T) {
	t.Run("holds the largest admitted burst", func(t *testing.T) {
		if sendChanSize <= keyBurst || sendChanSize <= defaultConnPublishBurst {
			t.Fatalf("sendChanSize is %d, want more than the per-key burst (%d) and the per-conn publish burst (%d)",
				sendChanSize, keyBurst, defaultConnPublishBurst)
		}
		q := newSendQueue()
		frame := make([]byte, 4096)
		for i := 0; i < keyBurst; i++ {
			if !q.offer(frame) {
				t.Fatalf("frame %d of a %d-frame burst of 4 KiB frames was refused", i+1, keyBurst)
			}
		}
	})

	t.Run("frame bound", func(t *testing.T) {
		q := newSendQueue()
		for i := 0; i < sendChanSize; i++ {
			if !q.offer([]byte("x")) {
				t.Fatalf("small frame %d refused, want %d accepted", i+1, sendChanSize)
			}
		}
		if q.offer([]byte("x")) {
			t.Fatalf("frame %d accepted, past the frame bound", sendChanSize+1)
		}
	})

	t.Run("byte bound", func(t *testing.T) {
		// 8 KiB frames reach the byte bound at 128, with half the slots free.
		q := newSendQueue()
		frame := make([]byte, 8192)
		n := 0
		for q.offer(frame) {
			n++
			if n > sendChanSize {
				t.Fatal("queue never refused a frame")
			}
		}
		if want := sendQueueBytes / len(frame); n != want {
			t.Fatalf("queue took %d frames of %d bytes, want %d", n, len(frame), want)
		}
		if got := q.bytes.Load(); got != int64(n*len(frame)) || got > sendQueueBytes {
			t.Fatalf("queue counts %d bytes for %d frames of %d, bound %d", got, n, len(frame), sendQueueBytes)
		}
		if q.offer([]byte("x")) {
			t.Fatal("one more byte accepted, past the byte bound")
		}
	})

	t.Run("first 64 frames of any size", func(t *testing.T) {
		q := newSendQueue()
		frame := make([]byte, maxFrame)
		for i := 0; i < sendUnmeteredFrames; i++ {
			if !q.offer(frame) {
				t.Fatalf("frame %d of %d bytes refused; the 64-slot queue took %d of any size", i+1, len(frame), sendUnmeteredFrames)
			}
		}
		if q.offer(frame) {
			t.Fatalf("frame %d of %d bytes accepted, past both the unmetered frames and the byte bound", sendUnmeteredFrames+1, len(frame))
		}
		if got, want := q.bytes.Load(), int64(sendUnmeteredFrames*maxFrame); got != want {
			t.Fatalf("queue counts %d bytes, want %d", got, want)
		}
	})

	t.Run("bytes come back when frames leave", func(t *testing.T) {
		q := newSendQueue()
		frame := make([]byte, maxFrame)
		for i := 0; i < sendUnmeteredFrames; i++ {
			q.offer(frame)
		}
		q.release(<-q.ch) // as writePump does
		if !q.offer(frame) {
			t.Fatal("frame refused after writePump took one")
		}
		if !q.evict() {
			t.Fatal("evict found nothing in a full queue")
		}
		if got, want := q.bytes.Load(), int64((sendUnmeteredFrames-1)*maxFrame); got != want {
			t.Fatalf("queue counts %d bytes after one eviction, want %d", got, want)
		}
		for q.evict() {
		}
		if got := q.bytes.Load(); got != 0 {
			t.Fatalf("empty queue counts %d bytes", got)
		}
	})
}

// TestSendPastByteBoundIsSlowConsumer ties the byte bound to the disconnect
// path: a conn whose queue has reached sendQueueBytes is a slow consumer even
// with frame slots to spare, so Send refuses the event and asks Run to close
// the conn.
func TestSendPastByteBoundIsSlowConsumer(t *testing.T) {
	c := &Conn{send: newSendQueue(), policy: PolicyDisconnect{}, closeReq: make(chan struct{}, 1)}
	frame := make([]byte, 8192)
	fits := sendQueueBytes / len(frame)
	for i := 0; i < fits; i++ {
		if err := c.Send(frame); err != nil {
			t.Fatalf("send %d of %d that fit the byte bound: %v", i+1, fits, err)
		}
	}
	if free := sendChanSize - len(c.send.ch); free <= 0 {
		t.Fatalf("no frame slots left (%d queued); this test needs the byte bound to be the one that trips", len(c.send.ch))
	}
	if err := c.Send(frame); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("send past the byte bound: got %v, want ErrSlowConsumer", err)
	}
	select {
	case <-c.closeReq:
	default:
		t.Fatal("Send did not ask Run to close the conn")
	}
}
