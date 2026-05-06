package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
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
	addr := ":8080"
	st := store.NewMemory()
	adminToken, err := auth.GenerateSecret()
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
	slog.Info("admin token (use as Bearer for /v1/keys)", "token", adminToken)
	reg := registry.NewSyncMap()
	fan := fanout.NewPerConn()
	rl := ratelimit.New(100, 200, time.Hour)
	defer rl.Close()
	h := hub.New()
	s := server.New(addr, st, adminToken, reg, signingSecret, fan, rl, conn.PolicyDisconnect{}, h)
	// Start stats publisher in background; goroutine exits when ctx is canceled.
	go hub.PublishStatsLoop(ctx, reg, 5*time.Second, func() map[string]int64 {
		// TODO: wire to actual prometheus collector values via testutil
		return map[string]int64{}
	})
	return s.Run(ctx)
}
