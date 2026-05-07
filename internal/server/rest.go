package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/store"
)

type RestHandler struct {
	store         store.Store
	adminToken    string
	signingSecret string
}

func NewRestHandler(s store.Store, adminToken, signingSecret string) *RestHandler {
	return &RestHandler{store: s, adminToken: adminToken, signingSecret: signingSecret}
}

func (h *RestHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/keys", h.requireAdmin(h.create))
	mux.HandleFunc("GET /v1/keys", h.requireAdmin(h.list))
	mux.HandleFunc("DELETE /v1/keys/{id}", h.requireAdmin(h.revoke))
	mux.HandleFunc("POST /v1/auth/sign", h.sign)
}

func (h *RestHandler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ah := r.Header.Get("Authorization")
		const p = "Bearer "
		if !strings.HasPrefix(ah, p) || ah[len(p):] != h.adminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *RestHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	secret, err := auth.GenerateSecret()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	k, err := h.store.CreateKey(r.Context(), body.Name, auth.HashSecret(secret))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": k.ID, "name": k.Name, "secret": secret})
}

func (h *RestHandler) list(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(keys)
}

func (h *RestHandler) revoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.store.RevokeKey(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RestHandler) sign(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
}
