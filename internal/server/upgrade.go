package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/EthanY33/wirefan/internal/conn"
	"github.com/EthanY33/wirefan/internal/fanout"
	"github.com/EthanY33/wirefan/internal/ratelimit"
	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/EthanY33/wirefan/internal/store"
	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"
)

type UpgradeHandler struct {
	store          store.Store
	allowedOrigins []string
	registry       registry.Registry
	signingSecret  string
	fanout         fanout.Fanout
	rateLimit      *ratelimit.Limiter
	policy         conn.Policy
}

func NewUpgradeHandler(st store.Store, origins []string, reg registry.Registry, signingSecret string, fan fanout.Fanout, rl *ratelimit.Limiter, pol conn.Policy) *UpgradeHandler {
	return &UpgradeHandler{
		store:          st,
		allowedOrigins: origins,
		registry:       reg,
		signingSecret:  signingSecret,
		fanout:         fan,
		rateLimit:      rl,
		policy:         pol,
	}
}

func (h *UpgradeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	keyID := r.URL.Query().Get("key")
	if keyID == "" {
		http.Error(w, "missing key", http.StatusUnauthorized)
		return
	}
	k, err := h.store.LookupKey(r.Context(), keyID)
	if err != nil || k.RevokedAt != nil {
		http.Error(w, "invalid key", http.StatusUnauthorized)
		return
	}
	opts := &websocket.AcceptOptions{OriginPatterns: h.allowedOrigins}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		if !errors.Is(err, http.ErrAbortHandler) {
			slog.Warn("ws upgrade failed", "err", err)
		}
		return
	}
	sid := ulid.Make().String()
	_ = conn.Run(r.Context(), c, sid, k.ID, h.registry, h.signingSecret, h.fanout, h.rateLimit, h.policy)
}
