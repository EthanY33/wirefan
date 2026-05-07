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

// Config controls the network surface and origin policy. addr is the
// public-facing listener (WS, /v1/auth/sign, health, static client).
// adminAddr is a separate listener for /metrics, /debug/pprof/*, and
// /v1/keys — keep this on loopback or an internal VLAN. allowedOrigins
// is the OriginPatterns slice handed to coder/websocket for the
// /v1/connect upgrade; "*" disables origin checking and should only
// appear in dev mode.
type Config struct {
	Addr           string
	AdminAddr      string
	AllowedOrigins []string
}

type Server struct {
	cfg      Config
	health   *HealthHandler
	mux      *http.ServeMux
	adminMux *http.ServeMux
	srv      *http.Server
	adminSrv *http.Server
	store    store.Store
	hub      *hub.Hub
}

// New builds the public and admin muxes. The admin listener is created
// only when cfg.AdminAddr is non-empty.
func New(cfg Config, st store.Store, adminToken string, reg registry.Registry, signingSecret string, fan fanout.Fanout, rl *ratelimit.Limiter, pol conn.Policy, h *hub.Hub) *Server {
	s := &Server{
		cfg:      cfg,
		health:   NewHealthHandler(),
		mux:      http.NewServeMux(),
		adminMux: http.NewServeMux(),
		store:    st,
		hub:      h,
	}

	rest := NewRestHandler(st, adminToken, signingSecret)

	// Public listener: health, /v1/connect (WS), /v1/auth/sign, static client.
	s.mux.Handle("/v1/health", s.health)
	rest.RegisterPublic(s.mux)
	s.mux.Handle("/v1/connect", NewUpgradeHandler(st, cfg.AllowedOrigins, reg, signingSecret, fan, rl, pol, h))
	s.mux.Handle("/", http.FileServerFS(web.Files))

	// Admin listener: metrics, pprof, key management. All gated by
	// requireAdmin AND bound to a separate (typically loopback) listener.
	metrics.Register()
	s.adminMux.Handle("/metrics", promhttp.Handler())
	s.adminMux.HandleFunc("/debug/pprof/", pprof.Index)
	s.adminMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	s.adminMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	s.adminMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	s.adminMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	rest.RegisterAdmin(s.adminMux)

	s.srv = &http.Server{Addr: cfg.Addr, Handler: s.mux}
	if cfg.AdminAddr != "" {
		s.adminSrv = &http.Server{Addr: cfg.AdminAddr, Handler: s.adminMux}
	}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() {
		slog.Info("public listening", "addr", s.cfg.Addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	if s.adminSrv != nil {
		go func() {
			slog.Info("admin listening", "addr", s.cfg.AdminAddr)
			if err := s.adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	s.health.SetDraining(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.hub.Drain(shutdownCtx, 30*time.Second)
	if s.adminSrv != nil {
		_ = s.adminSrv.Shutdown(shutdownCtx)
	}
	return s.srv.Shutdown(shutdownCtx)
}
