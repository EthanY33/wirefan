package conn

import "testing"

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
