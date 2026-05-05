package main

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func TestRunReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return within 2s of ctx cancel")
	}
}

var _ = syscall.SIGINT // keep import for later
