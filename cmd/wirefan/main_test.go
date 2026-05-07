package main

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/server"
)

func TestRunReturnsOnContextCancel(t *testing.T) {
	// :0 picks an ephemeral port — avoids "address already in use" when the
	// developer happens to have something on :8080, and prevents collision
	// between parallel test runs.
	cfg := server.Config{
		Addr:           "127.0.0.1:0",
		AdminAddr:      "127.0.0.1:0",
		AllowedOrigins: []string{"*"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
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

func TestParseFlagsRejectsMissingOrigins(t *testing.T) {
	if _, err := parseFlags([]string{}); err == nil {
		t.Fatal("expected error when --allowed-origins is missing")
	}
}

func TestParseFlagsRejectsStarWithoutDev(t *testing.T) {
	if _, err := parseFlags([]string{"--allowed-origins=*"}); err == nil {
		t.Fatal("expected error when --allowed-origins=* without --dev")
	}
}

func TestParseFlagsAcceptsExplicitOrigin(t *testing.T) {
	cfg, err := parseFlags([]string{"--allowed-origins=https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.AllowedOrigins) != 1 || cfg.AllowedOrigins[0] != "https://example.com" {
		t.Fatalf("unexpected origins: %v", cfg.AllowedOrigins)
	}
}

var _ = syscall.SIGINT // keep import for later
