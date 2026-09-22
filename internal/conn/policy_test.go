package conn

import (
	"testing"
	"time"
)

func TestPolicyDisconnect(t *testing.T) {
	p := PolicyDisconnect{}
	sent := false
	err := p.Apply(make(chan []byte), []byte("x"), func() { sent = true })
	if err != ErrSlowConsumer {
		t.Fatalf("want ErrSlowConsumer, got %v", err)
	}
	if sent {
		t.Fatal("disconnect should not send")
	}
}

func TestPolicyDropOldest(t *testing.T) {
	ch := make(chan []byte, 1)
	ch <- []byte("a")
	p := PolicyDropOldest{}
	if err := p.Apply(ch, []byte("b"), nil); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; string(got) != "b" {
		t.Fatalf("expected b, got %s", got)
	}
}

// TestPolicyDropOldestNeverBlocks is the G10 regression. After evicting the
// oldest message the buffer can still be full: sendAck and sendError write to
// c.send without taking sendMu and can take the freed slot. The retry used to
// be a blocking send, so Conn.Send stalled while holding sendMu (forever,
// once writePump had exited). An unbuffered channel is a buffer that stays
// full after any eviction; Apply must drop the message and return.
func TestPolicyDropOldestNeverBlocks(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- PolicyDropOldest{}.Apply(make(chan []byte), []byte("x"), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Apply blocked on a buffer that stayed full after eviction")
	}
}

func TestPolicyDropNewest(t *testing.T) {
	ch := make(chan []byte, 1)
	ch <- []byte("a")
	p := PolicyDropNewest{}
	if err := p.Apply(ch, []byte("b"), nil); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; string(got) != "a" {
		t.Fatalf("expected a, got %s", got)
	}
}
