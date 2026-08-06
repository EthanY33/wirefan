package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
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

// Deps bundles the long-lived dependencies a Server stitches together. All
// fields are required.
type Deps struct {
	Store         store.Store
	AdminToken    string
	Registry      registry.Registry
	SigningSecret string
	Fanout        fanout.Fanout
	RateLimit     *ratelimit.Limiter
	Policy        conn.Policy
	Hub           *hub.Hub
}

type Server struct {
	cfg         Config
	health      *HealthHandler
	mux         *http.ServeMux
	adminMux    *http.ServeMux
	srv         *http.Server
	adminSrv    *http.Server
	store       store.Store
	hub         *hub.Hub
	fan         fanout.Fanout
	replayCache *auth.ReplayCache
}

// New builds the public and admin muxes. The admin listener is created
// only when cfg.AdminAddr is non-empty.
func New(cfg Config, deps Deps) *Server {
	rc := auth.NewReplayCache()
	s := &Server{
		cfg:         cfg,
		health:      NewHealthHandler(),
		mux:         http.NewServeMux(),
		adminMux:    http.NewServeMux(),
		store:       deps.Store,
		hub:         deps.Hub,
		fan:         deps.Fanout,
		replayCache: rc,
	}

	rest := NewRestHandler(deps.Store, deps.AdminToken, deps.SigningSecret)

	// Public listener: health, /v1/connect (WS), /v1/auth/sign, static client.
	s.mux.Handle("/v1/health", s.health)
	rest.RegisterPublic(s.mux)
	s.mux.Handle("/v1/connect", NewUpgradeHandler(UpgradeDeps{
		Store:          deps.Store,
		AllowedOrigins: cfg.AllowedOrigins,
		Registry:       deps.Registry,
		SigningSecret:  deps.SigningSecret,
		ReplayCache:    rc,
		Fanout:         deps.Fanout,
		RateLimit:      deps.RateLimit,
		Policy:         deps.Policy,
		Hub:            deps.Hub,
	}))
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

	// ReadHeaderTimeout caps how long a peer can dribble in request headers
	// (slowloris). IdleTimeout reaps keep-alive connections that never send
	// another request. ReadTimeout / WriteTimeout are intentionally unset:
	// /v1/connect is a long-lived WS upgrade — a body-read deadline here
	// would force a reconnect every N seconds, and writes are paced by
	// websocket.Conn's own per-message deadlines (see internal/conn/conn.go).
	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if cfg.AdminAddr != "" {
		s.adminSrv = &http.Server{
			Addr:              cfg.AdminAddr,
			Handler:           s.adminMux,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
	}
	return s
}

// ReplayCache exposes the per-Server token replay cache so the caller can
// run a periodic Sweep goroutine. Public so cmd/wirefan/main.go can drive
// the sweeper without exporting a separate accessor.
func (s *Server) ReplayCache() *auth.ReplayCache { return s.replayCache }

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
	go s.sweepReplayCache(ctx)

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
	err := s.srv.Shutdown(shutdownCtx)
	// Stop fanout workers last. Draining conns is not enough on its own: the
	// public listener still accepts /v1/connect upgrades until Shutdown
	// returns, so closing earlier leaves a window where a fresh conn can
	// publish into a closed ShardedPool, whose Broadcast is a silent no-op
	// after Close while metrics still count the publish as delivered. Once
	// the listener is down no new broadcast can arrive, and Close waits for
	// queued ones so the goroutine-leak invariant still holds.
	if s.fan != nil {
		_ = s.fan.Close()
	}
	return err
}

// sweepReplayCache evicts expired token jti entries every minute. Memory in
// the cache is bounded by the issuance rate * token lifetime (5 minutes by
// default), so a sweep cadence of one minute gives at most ~5 minutes of
// expired entries before reclamation. Loops until ctx is canceled.
func (s *Server) sweepReplayCache(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.replayCache.Sweep()
		}
	}
}
