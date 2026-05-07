package server

import (
	"net/http"
	"sync/atomic"
)

type HealthHandler struct {
	draining atomic.Bool
}

func NewHealthHandler() *HealthHandler { return &HealthHandler{} }

func (h *HealthHandler) SetDraining(d bool) { h.draining.Store(d) }

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
