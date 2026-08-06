package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/EthanY33/wirefan/internal/server"
	"github.com/coder/websocket"
)

func TestRunReturnsOnContextCancel(t *testing.T) {
	// :0 picks an ephemeral port — avoids "address already in use" when the
	// developer happens to have something on :8080, and prevents collision
	// between parallel test runs.
	cfg := appConfig{
		srv: server.Config{
			Addr:           "127.0.0.1:0",
			AdminAddr:      "127.0.0.1:0",
			AllowedOrigins: []string{"*"},
		},
		store: "memory",
	}
	t.Setenv("WIREFAN_STATE_DIR", t.TempDir())
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
	if len(cfg.srv.AllowedOrigins) != 1 || cfg.srv.AllowedOrigins[0] != "https://example.com" {
		t.Fatalf("unexpected origins: %v", cfg.srv.AllowedOrigins)
	}
}

func TestParseFlagsStoreDefaultsAndValidation(t *testing.T) {
	cfg, err := parseFlags([]string{"--dev", "--allowed-origins=*"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.store != "sqlite" {
		t.Fatalf("default store = %q, want sqlite", cfg.store)
	}
	if cfg.dbPath != "" {
		t.Fatalf("default db path = %q, want empty (resolved at open time)", cfg.dbPath)
	}
	cfg, err = parseFlags([]string{"--dev", "--allowed-origins=*", "--store=memory", "--db-path=/tmp/x.db"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.store != "memory" || cfg.dbPath != "/tmp/x.db" {
		t.Fatalf("got store=%q dbPath=%q", cfg.store, cfg.dbPath)
	}
	if _, err := parseFlags([]string{"--dev", "--allowed-origins=*", "--store=bogus"}); err == nil {
		t.Fatal("expected error for --store=bogus")
	}
}

// freeAddr reserves an ephemeral loopback port and releases it so the server
// under test can bind it. Small race window between Close and the server's
// Listen, acceptable for a test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/v1/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy", addr)
}

// TestSQLiteKeyPersistsAcrossRestart is the headline behavior change of the
// SQLite default: mint a key via the admin API, stop the process, boot a new
// one against the same --db-path, and the key must still authenticate a
// WebSocket connect.
func TestSQLiteKeyPersistsAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("WIREFAN_STATE_DIR", stateDir)
	t.Setenv("WIREFAN_ADMIN_TOKEN", "test-admin-token")
	pubAddr, admAddr := freeAddr(t), freeAddr(t)
	cfg := appConfig{
		srv: server.Config{
			Addr:           pubAddr,
			AdminAddr:      admAddr,
			AllowedOrigins: []string{"*"},
		},
		store:  "sqlite",
		dbPath: filepath.Join(stateDir, "wirefan.db"),
	}

	// Boot #1: mint a key.
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- run(ctx1, cfg) }()
	waitHealthy(t, pubAddr)

	req, _ := http.NewRequest(http.MethodPost, "http://"+admAddr+"/v1/keys", strings.NewReader(`{"name":"persist-test"}`))
	req.Header.Set("Authorization", "Bearer test-admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel1()
		t.Fatalf("mint key: %v", err)
	}
	var minted struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		_ = resp.Body.Close()
		cancel1()
		t.Fatalf("decode mint response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		cancel1()
		t.Fatalf("mint key status = %d", resp.StatusCode)
	}
	if minted.ID == "" {
		cancel1()
		t.Fatal("mint response has empty id")
	}

	cancel1()
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("first server did not shut down")
	}

	// Boot #2: same db path, the minted key must authenticate /v1/connect.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- run(ctx2, cfg) }()
	waitHealthy(t, pubAddr)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	c, _, err := websocket.Dial(dialCtx, "ws://"+pubAddr+"/v1/connect?key="+minted.ID, nil)
	if err != nil {
		t.Fatalf("key minted before restart failed to authenticate after restart: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("second server did not shut down")
	}
}

var _ = syscall.SIGINT // keep import for later
