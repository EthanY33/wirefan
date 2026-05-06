package ratelimit

import (
	"testing"
	"time"
)

func TestAllowAndExhaust(t *testing.T) {
	l := New(10, 5, time.Hour)
	defer l.Close()
	for i := 0; i < 5; i++ {
		if !l.Allow("key-a") {
			t.Fatalf("expected allowed at %d", i)
		}
	}
	if l.Allow("key-a") {
		t.Fatal("expected denied after burst")
	}
}

func TestPerKeyIsolation(t *testing.T) {
	l := New(10, 1, time.Hour)
	defer l.Close()
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("first burst per key allowed")
	}
}

func TestGCEvictsStale(t *testing.T) {
	l := New(10, 1, 10*time.Millisecond)
	defer l.Close()
	l.Allow("k")
	time.Sleep(50 * time.Millisecond)
	l.gcOnce()
	if _, ok := l.peek("k"); ok {
		t.Fatal("expected k evicted")
	}
}
