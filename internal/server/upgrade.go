package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
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
	trustedProxies []netip.Prefix

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
		trustedProxies: parseTrustedProxies(os.Getenv("WIREFAN_TRUSTED_PROXIES")),
		ipCount:        map[string]int{},
		ipCap:          defaultIPCap,
	}
}

// sanitizeLogValue strips characters that could let an attacker inject a fake
// log line via reflected headers (CR, LF, NUL, vertical-tab, escape, DEL).
// Returns the value unchanged when no such characters appear, which is the
// hot path for clean errors. Limited to 1 KiB so a giant request header
// can't blow up the log volume on a sustained 4xx loop.
func sanitizeLogValue(s string) string {
	const max = 1024
	if len(s) > max {
		s = s[:max]
	}
	clean := true
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0 || r == 0x1b || r == 0x7f || r < 0x20 {
			if r != '\t' {
				clean = false
				break
			}
		}
	}
	if clean {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0 || r == 0x1b || r == 0x7f || (r < 0x20 && r != '\t') {
			out = append(out, '?')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// parseTrustedProxies turns a comma-separated CIDR / single-IP list into
// netip.Prefix values. Bare IPs (e.g. "127.0.0.1") become /32 or /128.
// Malformed entries are silently dropped — operators should verify with a
// smoke test, and a hard failure here would block boot on a typo without
// a config-validation tool to backstop it.
func parseTrustedProxies(raw string) []netip.Prefix {
	if raw == "" {
		return nil
	}
	var prefixes []netip.Prefix
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			prefixes = append(prefixes, p)
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			bits := 32
			if a.Is6() {
				bits = 128
			}
			if p, err := a.Prefix(bits); err == nil {
				prefixes = append(prefixes, p)
			}
		}
	}
	return prefixes
}

func addrInPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// stripHostPort handles "1.2.3.4:5678", "[::1]:5678", "[::1]", and "::1".
// Returns the address part as a netip.Addr, plus the original string form.
func stripHostPort(s string) (netip.Addr, string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, "", false
	}
	host := s
	if strings.HasPrefix(s, "[") {
		if end := strings.LastIndex(s, "]"); end > 0 {
			host = s[1:end]
		}
	} else if i := strings.LastIndex(s, ":"); i > 0 && strings.Count(s, ":") == 1 {
		// IPv4 with port. Bare IPv6 has multiple colons; leave it alone.
		host = s[:i]
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host, false
	}
	return a, host, true
}

// clientIP returns a best-effort source IP. When the request's RemoteAddr is
// in trustedProxies, the X-Forwarded-For header is consulted using the
// rightmost-untrusted-hop algorithm: walk the XFF list right to left and
// return the first hop whose IP is not in trustedProxies. This is the
// algorithm called for in CWE-348 mitigations (do NOT use the leftmost
// entry — clients can pick that themselves).
func clientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	raAddr, raStr, raOk := stripHostPort(r.RemoteAddr)
	if !raOk {
		return r.RemoteAddr
	}
	if len(trustedProxies) == 0 || !addrInPrefixes(raAddr, trustedProxies) {
		return raStr
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return raStr
	}
	hops := strings.Split(xff, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hopAddr, hopStr, ok := stripHostPort(hops[i])
		if !ok {
			continue
		}
		if !addrInPrefixes(hopAddr, trustedProxies) {
			return hopStr
		}
	}
	// Every XFF hop was trusted; treat the leftmost as the originator.
	if leftAddr, leftStr, ok := stripHostPort(hops[0]); ok {
		_ = leftAddr
		return leftStr
	}
	return raStr
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
	ip := clientIP(r, h.trustedProxies)
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
			slog.Warn("ws upgrade failed", "err", sanitizeLogValue(err.Error()))
		}
		return
	}
	sid := ulid.Make().String()
	_ = conn.Run(r.Context(), c, sid, k.ID, h.registry, h.signingSecret, h.fanout, h.rateLimit, h.policy, h.hub)
}
