package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/store"
	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"
)

// defaultIPCap is the per-source-IP active-connection ceiling. Anyone trying
// to open more than this many sockets from the same IP gets 429s. Sized to
// be well above what a real human juggling browser tabs would generate, but
// well below the kind of fan-out a single phantom-conn loop can issue.
const defaultIPCap = 200

type UpgradeHandler struct {
	store          store.Store
	allowedOrigins []string
	registry       registry.Registry
	signingSecret  string
	fanout         fanout.Fanout
	rateLimit      *ratelimit.Limiter
	policy         conn.Policy
	hub            *hub.Hub

	ipMu    sync.Mutex
	ipCount map[string]int
	ipCap   int
}

func NewUpgradeHandler(st store.Store, origins []string, reg registry.Registry, signingSecret string, fan fanout.Fanout, rl *ratelimit.Limiter, pol conn.Policy, h *hub.Hub) *UpgradeHandler {
	return &UpgradeHandler{
		store:          st,
		allowedOrigins: origins,
		registry:       reg,
		signingSecret:  signingSecret,
		fanout:         fan,
		rateLimit:      rl,
		policy:         pol,
		hub:            h,
		ipCount:        map[string]int{},
		ipCap:          defaultIPCap,
	}
}

// clientIP extracts a best-effort source IP. Behind a trusted proxy the caller
// would want X-Forwarded-For, but for the direct-connect demo path RemoteAddr
// is fine. Strips the port suffix; handles IPv6 (e.g. "[::1]:1234") by
// preserving the bracketed host as the key.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	// IPv6: "[::1]:1234"
	if strings.HasPrefix(addr, "[") {
		if end := strings.LastIndex(addr, "]"); end > 0 {
			return addr[:end+1]
		}
		return addr
	}
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func (h *UpgradeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	keyID := r.URL.Query().Get("key")
	if keyID == "" {
		metrics.UpgradeRej.WithLabelValues("bad_key").Inc()
		http.Error(w, "missing key", http.StatusUnauthorized)
		return
	}
	k, err := h.store.LookupKey(r.Context(), keyID)
	if err != nil || k.RevokedAt != nil {
		metrics.UpgradeRej.WithLabelValues("bad_key").Inc()
		http.Error(w, "invalid key", http.StatusUnauthorized)
		return
	}

	// Per-source-IP active-connection cap. Phantom-conn loops are the threat
	// model; we count ALL conns on the IP, not just phantom ones (a real
	// browser will only ever have a couple of tabs open at once, so the
	// distinction doesn't matter in practice).
	ip := clientIP(r)
	h.ipMu.Lock()
	if h.ipCount[ip] >= h.ipCap {
		h.ipMu.Unlock()
		metrics.UpgradeRej.WithLabelValues("phantom_cap").Inc()
		http.Error(w, "too many connections from this IP", http.StatusTooManyRequests)
		return
	}
	h.ipCount[ip]++
	h.ipMu.Unlock()
	defer func() {
		h.ipMu.Lock()
		h.ipCount[ip]--
		if h.ipCount[ip] <= 0 {
			delete(h.ipCount, ip)
		}
		h.ipMu.Unlock()
	}()

	opts := &websocket.AcceptOptions{OriginPatterns: h.allowedOrigins}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		if !errors.Is(err, http.ErrAbortHandler) {
			slog.Warn("ws upgrade failed", "err", err)
		}
		return
	}
	sid := ulid.Make().String()
	_ = conn.Run(r.Context(), c, sid, k.ID, h.registry, h.signingSecret, h.fanout, h.rateLimit, h.policy, h.hub)
}
