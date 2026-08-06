package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/store"
)

// maxAdminBodyBytes caps inbound JSON for admin POST handlers. The admin
// surface only takes `{"name": "..."}`-shaped payloads, so 64 KiB is two
// orders of magnitude above legitimate use and well below the heap pressure
// an attacker could induce by spamming /v1/keys with multi-MB bodies.
const maxAdminBodyBytes = 1 << 16

type RestHandler struct {
	store         store.Store
	adminToken    string
	signingSecret string
}

func NewRestHandler(s store.Store, adminToken, signingSecret string) *RestHandler {
	return &RestHandler{store: s, adminToken: adminToken, signingSecret: signingSecret}
}

// keyView is the public projection of store.Key returned by GET /v1/keys.
// Crucially it omits SecretHash. The hash is unsalted SHA-256 over a
// 256-bit random secret, which is the right construction here: the input
// is full-entropy random rather than a human password, so there is nothing
// for a dictionary attack to exploit. The hash is still server-internal
// and has no business appearing in an admin list response.
type keyView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func toKeyView(k store.Key) keyView {
	return keyView{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt}
}

// Register mounts both public and admin endpoints on a single mux. Kept
// for callers that don't separate the two listeners.
func (h *RestHandler) Register(mux *http.ServeMux) {
	h.RegisterPublic(mux)
	h.RegisterAdmin(mux)
}

// RegisterPublic mounts endpoints reachable on the public listener. Today
// only /v1/auth/sign — it does its own credential check internally.
func (h *RestHandler) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/sign", h.sign)
}

// RegisterAdmin mounts admin-only endpoints. The handlers gate themselves
// with requireAdmin, but operators should additionally bind these on a
// separate non-public listener (--admin-addr) so a misconfigured ingress
// can't expose them by accident.
func (h *RestHandler) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/keys", h.requireAdmin(h.create))
	mux.HandleFunc("GET /v1/keys", h.requireAdmin(h.list))
	mux.HandleFunc("DELETE /v1/keys/{id}", h.requireAdmin(h.revoke))
}

func (h *RestHandler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ah := r.Header.Get("Authorization")
		const p = "Bearer "
		if !strings.HasPrefix(ah, p) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(ah[len(p):])
		want := []byte(h.adminToken)
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// writeJSON serializes v as JSON with the correct content-type. Errors during
// encoding are logged but cannot be reported to the client — the status code
// has already been committed.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("rest: encode response", "err", err)
	}
}

func (h *RestHandler) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	secret, err := auth.GenerateSecret()
	if err != nil {
		slog.Error("rest: generate secret", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	k, err := h.store.CreateKey(r.Context(), body.Name, auth.HashSecret(secret))
	if err != nil {
		slog.Error("rest: create key", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": k.ID, "name": k.Name, "secret": secret})
}

func (h *RestHandler) list(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListKeys(r.Context())
	if err != nil {
		slog.Error("rest: list keys", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]keyView, len(keys))
	for i, k := range keys {
		views[i] = toKeyView(k)
	}
	writeJSON(w, http.StatusOK, views)
}

func (h *RestHandler) revoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.store.RevokeKey(r.Context(), id)
	if errors.Is(err, store.ErrKeyNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("rest: revoke key", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RestHandler) sign(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	ah := r.Header.Get("Authorization")
	creds := strings.TrimPrefix(ah, "Bearer ")
	parts := strings.SplitN(creds, ":", 2)
	if len(parts) != 2 {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	k, err := h.store.LookupKey(r.Context(), parts[0])
	if err != nil || k.RevokedAt != nil || !auth.VerifySecret(parts[1], k.SecretHash) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	var body struct {
		SocketID string `json:"socket_id"`
		Channel  string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := auth.SignToken(h.signingSecret, body.SocketID, body.Channel, time.Now().Add(5*time.Minute))
	if err != nil {
		slog.Error("rest: sign token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}
