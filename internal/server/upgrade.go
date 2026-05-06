package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/EthanY33/wirefan/internal/conn"
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
}

func NewUpgradeHandler(st store.Store, origins []string, reg registry.Registry, signingSecret string) *UpgradeHandler {
	return &UpgradeHandler{
		store:          st,
		allowedOrigins: origins,
		registry:       reg,
		signingSecret:  signingSecret,
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
	_ = conn.Run(r.Context(), c, sid, h.registry, h.signingSecret)
}
