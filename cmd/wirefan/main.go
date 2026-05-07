package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/server"
	"github.com/EthanY33/wirefan/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}
	st := store.NewMemory()
	adminToken, err := resolveAdminToken()
	if err != nil {
		return err
	}
	signingSecret, err := auth.GenerateSecret()
	if err != nil {
		return err
	}
	// OTel is dormant when endpoint is empty — returns a no-op shutdown.
	otelShutdown, err := metrics.InitOTel(ctx, "")
	if err != nil {
		return err
	}
	defer func() { _ = otelShutdown(context.Background()) }()
	reg := registry.NewSyncMap()
	fan := fanout.NewPerConn()
	rl := ratelimit.New(100, 200, time.Hour)
	defer rl.Close()
	h := hub.New()
	s := server.New(cfg, st, adminToken, reg, signingSecret, fan, rl, conn.PolicyDisconnect{}, h)
	// Start stats publisher in background; goroutine exits when ctx is canceled.
	go hub.PublishStatsLoop(ctx, reg, 5*time.Second, func() map[string]int64 {
		// TODO: wire to actual prometheus collector values via testutil
		return map[string]int64{}
	})
	return s.Run(ctx)
}

// resolveAdminToken returns the admin bearer token for /v1/keys, in priority
// order:
//
//  1. WIREFAN_ADMIN_TOKEN environment variable, if non-empty. The operator is
//     responsible for keeping the value out of process-listing tools.
//  2. The contents of $WIREFAN_STATE_DIR/admin.token (or
//     ./var/admin.token), if the file exists.
//  3. A freshly-generated 32-byte hex token, written to that file with mode
//     0600 so subsequent boots reuse it.
//
// The token is never logged. Operators retrieve it by reading the file or
// supplying it via env. This replaces the previous behavior of logging the
// freshly-generated token at slog.Info on every boot, which leaked it to
// journald / container stdout / log aggregators.
func resolveAdminToken() (string, error) {
	if t := os.Getenv("WIREFAN_ADMIN_TOKEN"); t != "" {
		return t, nil
	}
	stateDir := os.Getenv("WIREFAN_STATE_DIR")
	if stateDir == "" {
		stateDir = "var"
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	tokenPath := filepath.Join(stateDir, "admin.token")
	if data, err := os.ReadFile(tokenPath); err == nil {
		t := string(data)
		// Trim trailing newline if the file was hand-edited.
		for len(t) > 0 && (t[len(t)-1] == '\n' || t[len(t)-1] == '\r' || t[len(t)-1] == ' ') {
			t = t[:len(t)-1]
		}
		if t != "" {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	t, err := auth.GenerateSecret()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tokenPath, []byte(t), 0o600); err != nil {
		return "", err
	}
	slog.Info("admin token written", "path", tokenPath)
	return t, nil
}

// parseFlags binds the listener and origin policy from CLI flags. Default
// for --listen preserves the pre-flag :8080 behavior. The admin listener
// defaults to 127.0.0.1:6060 so /metrics and /debug/pprof/* are no longer
// reachable from a misconfigured ingress that bypasses the reverse proxy.
//
// --allowed-origins is required: refusing to start when it is "*" outside
// --dev makes "skip origin check" an explicit choice rather than the
// silent default it used to be.
func parseFlags() (server.Config, error) {
	fs := flag.NewFlagSet("wirefan", flag.ContinueOnError)
	addr := fs.String("listen", ":8080", "public listener address (WS, /v1/auth/sign, /v1/health, static client)")
	adminAddr := fs.String("admin-addr", "127.0.0.1:6060", "admin listener address (/metrics, /debug/pprof/*, /v1/keys); empty disables")
	origins := fs.String("allowed-origins", "", "comma-separated WebSocket Origin allowlist (e.g. https://example.com,https://staging.example.com); '*' is rejected outside --dev")
	dev := fs.Bool("dev", false, "developer mode: permits --allowed-origins=*")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return server.Config{}, err
	}
	if *origins == "" {
		return server.Config{}, errors.New("--allowed-origins is required (use --allowed-origins=https://your.host or pass --dev with --allowed-origins=*)")
	}
	parts := strings.Split(*origins, ",")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return server.Config{}, errors.New("--allowed-origins parsed to empty list")
	}
	if !*dev {
		for _, o := range cleaned {
			if o == "*" {
				return server.Config{}, errors.New("--allowed-origins=* requires --dev")
			}
		}
	}
	return server.Config{
		Addr:           *addr,
		AdminAddr:      *adminAddr,
		AllowedOrigins: cleaned,
	}, nil
}
