package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
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
	slog.Info("admin token (use as Bearer for /v1/keys)", "token", adminToken)
	reg := registry.NewSyncMap()
	fan := fanout.NewPerConn()
	rl := ratelimit.New()
	s := server.New(addr, st, adminToken, reg, signingSecret, fan, rl, conn.PolicyDisconnect{})
	return s.Run(ctx)
}
