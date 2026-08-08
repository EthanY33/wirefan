package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

// appConfig is the full process configuration: the server's network surface
// plus the implementation selections (store, registry, fanout) that main
// wires together before handing deps to server.New.
type appConfig struct {
	srv      server.Config
	store    string // "sqlite" | "memory"
	dbPath   string // sqlite file path; empty means <state-dir>/wirefan.db
	registry string // "sync-map" | "sharded"
	fanout   string // "per-conn" | "sharded"
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// openStore selects the key store per --store. SQLite is the default so
// minted API keys survive restarts; --store=memory keeps the old wipe-on-boot
// behavior for tests and hermetic benchmark cells. The SQLite path is
// resolved to an absolute path because store.NewSQLite rejects relative
// paths (DSN-injection guard).
func openStore(cfg appConfig) (store.Store, error) {
	switch cfg.store {
	case "memory":
		return store.NewMemory(), nil
	case "sqlite":
		p := cfg.dbPath
		if p == "" {
			dir, err := ensureStateDir()
			if err != nil {
				return nil, err
			}
			p = filepath.Join(dir, "wirefan.db")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, err
		}
		return store.NewSQLite(abs)
	default:
		return nil, errors.New("unknown store: " + cfg.store)
	}
}

// newRegistry and newFanout select the channel-registry and fanout
// implementations per --registry/--fanout. Each pair implements the same
// interface and is independently tested (the registries share one test
// suite; each fanout has its own tests). The flags exist so the benchmark
// matrix (docs/BENCHMARKS.md) exercises real binaries, not hand-edited
// builds.
func newRegistry(kind string) (registry.Registry, error) {
	switch kind {
	case "sync-map":
		return registry.NewSyncMap(), nil
	case "sharded":
		return registry.NewSharded(), nil
	default:
		return nil, errors.New("unknown registry: " + kind)
	}
}

func newFanout(kind string) (fanout.Fanout, error) {
	switch kind {
	case "per-conn":
		return fanout.NewPerConn(), nil
	case "sharded":
		return fanout.NewShardedPool(runtime.GOMAXPROCS(0)), nil
	default:
		return nil, errors.New("unknown fanout: " + kind)
	}
}

func run(ctx context.Context, cfg appConfig) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
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
	reg, err := newRegistry(cfg.registry)
	if err != nil {
		return err
	}
	fan, err := newFanout(cfg.fanout)
	if err != nil {
		return err
	}
	// server.Run closes the fanout on graceful shutdown; this defer covers
	// early-error returns before Run starts. Close is idempotent.
	defer func() { _ = fan.Close() }()
	rl := ratelimit.New(100, 200, time.Hour)
	defer rl.Close()
	h := hub.New()
	s := server.New(cfg.srv, server.Deps{
		Store:         st,
		AdminToken:    adminToken,
		Registry:      reg,
		SigningSecret: signingSecret,
		Fanout:        fan,
		RateLimit:     rl,
		Policy:        conn.PolicyDisconnect{},
		Hub:           h,
	})
	// Stats publisher: emits live gauges on the _wirefan-stats channel.
	// Every value, channel count included, comes from the same collectors
	// /metrics exposes, so the stats channel and the Prometheus endpoint
	// cannot report different numbers.
	metrics.SetChannelSource(reg.Len)
	go hub.PublishStatsLoop(ctx, reg, 5*time.Second, metrics.SnapshotBasic)
	// Channel GC: removes channels that have lost all their subscribers.
	// Sub.Subscribe verifies Deleted under SubsMu, so the GC and a racing
	// subscribe synchronise correctly without needing a registry-wide lock.
	go registry.SweepLoop(ctx, reg, registry.DefaultSweepInterval)
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
	stateDir, err := ensureStateDir()
	if err != nil {
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

// ensureStateDir resolves the state directory ($WIREFAN_STATE_DIR, falling
// back to ./var) and creates it with 0700 so only the service user can read
// the admin token and key database it will contain.
func ensureStateDir() (string, error) {
	dir := os.Getenv("WIREFAN_STATE_DIR")
	if dir == "" {
		dir = "var"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// parseFlags binds the listener and origin policy from CLI flags. Default
// for --listen preserves the pre-flag :8080 behavior. The admin listener
// defaults to 127.0.0.1:6060 so /metrics and /debug/pprof/* are no longer
// reachable from a misconfigured ingress that bypasses the reverse proxy.
//
// --allowed-origins is required: refusing to start when it is "*" outside
// --dev makes "skip origin check" an explicit choice rather than the
// silent default it used to be.
func parseFlags(args []string) (appConfig, error) {
	fs := flag.NewFlagSet("wirefan", flag.ContinueOnError)
	addr := fs.String("listen", ":8080", "public listener address (WS, /v1/auth/sign, /v1/health, static client)")
	adminAddr := fs.String("admin-addr", "127.0.0.1:6060", "admin listener address (/metrics, /debug/pprof/*, /v1/keys); empty disables")
	origins := fs.String("allowed-origins", "", "comma-separated WebSocket Origin allowlist (e.g. https://example.com,https://staging.example.com); '*' is rejected outside --dev")
	dev := fs.Bool("dev", false, "developer mode: permits --allowed-origins=*")
	storeKind := fs.String("store", "sqlite", "key store backend: sqlite (persistent, default) or memory (wiped on restart)")
	dbPath := fs.String("db-path", "", "sqlite database file (default <state-dir>/wirefan.db; state dir is $WIREFAN_STATE_DIR or ./var)")
	registryKind := fs.String("registry", "sync-map", "channel registry implementation: sync-map or sharded")
	fanoutKind := fs.String("fanout", "per-conn", "fanout implementation: per-conn or sharded (worker pool sized to GOMAXPROCS)")
	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}
	if *storeKind != "sqlite" && *storeKind != "memory" {
		return appConfig{}, errors.New("--store must be sqlite or memory")
	}
	if *registryKind != "sync-map" && *registryKind != "sharded" {
		return appConfig{}, errors.New("--registry must be sync-map or sharded")
	}
	if *fanoutKind != "per-conn" && *fanoutKind != "sharded" {
		return appConfig{}, errors.New("--fanout must be per-conn or sharded")
	}
	if *origins == "" {
		return appConfig{}, errors.New("--allowed-origins is required (use --allowed-origins=https://your.host or pass --dev with --allowed-origins=*)")
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
		return appConfig{}, errors.New("--allowed-origins parsed to empty list")
	}
	if !*dev {
		for _, o := range cleaned {
			if o == "*" {
				return appConfig{}, errors.New("--allowed-origins=* requires --dev")
			}
		}
	}
	return appConfig{
		srv: server.Config{
			Addr:           *addr,
			AdminAddr:      *adminAddr,
			AllowedOrigins: cleaned,
		},
		store:    *storeKind,
		dbPath:   *dbPath,
		registry: *registryKind,
		fanout:   *fanoutKind,
	}, nil
}
