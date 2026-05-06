package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/store"
	"github.com/EthanY33/wirefan/web"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	addr   string
	health *HealthHandler
	mux    *http.ServeMux
	srv    *http.Server
	store  store.Store
	hub    *hub.Hub
}

func New(addr string, st store.Store, adminToken string, reg registry.Registry, signingSecret string, fan fanout.Fanout, rl *ratelimit.Limiter, pol conn.Policy, h *hub.Hub) *Server {
	s := &Server{addr: addr, health: NewHealthHandler(), mux: http.NewServeMux(), store: st, hub: h}
	s.mux.Handle("/v1/health", s.health)
	NewRestHandler(st, adminToken, signingSecret).Register(s.mux)
	s.mux.Handle("/v1/connect", NewUpgradeHandler(st, []string{"*"}, reg, signingSecret, fan, rl, pol, h))

	metrics.Register()
	s.mux.Handle("/metrics", promhttp.Handler())
	s.mux.HandleFunc("/debug/pprof/", pprof.Index)
	s.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	s.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Static demo client (served from embed.FS). Mounted at "/" so it's the
	// fallback for any unmatched path; the more-specific API/metrics/pprof
	// routes above take precedence.
	s.mux.Handle("/", http.FileServerFS(web.Files))

	s.srv = &http.Server{Addr: addr, Handler: s.mux}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", s.addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	s.health.SetDraining(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.hub.Drain(shutdownCtx, 30*time.Second)
	return s.srv.Shutdown(shutdownCtx)
}
