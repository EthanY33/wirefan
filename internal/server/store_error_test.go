package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/hub"
	"github.com/EthanY33/wirefan/internal/metrics"
	"github.com/EthanY33/wirefan/internal/store"
	dto "github.com/prometheus/client_model/go"
)

// failingLookupStore is a Store whose LookupKey always fails with err, the
// way the SQLite store does when the database stays locked past its busy
// timeout or the disk returns an I/O error. Every other method is the
// embedded store's.
type failingLookupStore struct {
	store.Store
	err error
}

func (f failingLookupStore) LookupKey(context.Context, string) (store.Key, error) {
	return store.Key{}, f.err
}

// lockedBuffer is a bytes.Buffer safe to write from handler goroutines
// while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs points the default slog logger at a buffer until the test
// ends. slog.SetDefault also rewires the standard log package, so its
// writer and flags are put back as well.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	prevLogger := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	buf := &lockedBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return buf
}

// upgradeRejected reads wirefan_upgrade_rejected_total for one reason. The
// counter is process-wide, so callers compare before and after.
func upgradeRejected(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.UpgradeRej.WithLabelValues(reason).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// TestUpgradeKeyStoreFailureIsNotABadKey: when the key store cannot be
// read, /v1/connect used to answer 401 "invalid key", count the request
// under reason="bad_key" and log nothing. A transient storage fault looked
// to the client like a credential it must not retry and to the operator
// like someone guessing keys. It is a 503, its own reason, and an error in
// the log.
func TestUpgradeKeyStoreFailureIsNotABadKey(t *testing.T) {
	logs := captureLogs(t)
	h := NewUpgradeHandler(UpgradeDeps{
		Store: failingLookupStore{store.NewMemory(), errors.New("disk I/O error")},
	})
	badKey, storeErr := upgradeRejected(t, "bad_key"), upgradeRejected(t, "store_error")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/connect?key=01J8Z4Q0M3W6B9XKAV5T2C7RPD", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d %q, want %d", rec.Code, strings.TrimSpace(rec.Body.String()), http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "key store unavailable" {
		t.Errorf("body = %q, want %q", got, "key store unavailable")
	}
	if d := upgradeRejected(t, "bad_key") - badKey; d != 0 {
		t.Errorf("bad_key counter moved by %v on a store failure, want 0", d)
	}
	if d := upgradeRejected(t, "store_error") - storeErr; d != 1 {
		t.Errorf("store_error counter moved by %v, want 1", d)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "disk I/O error") {
		t.Errorf("store failure was not logged as an error; log output: %q", out)
	}
}

// TestUpgradeUnknownOrRevokedKeyIsStill401 pins the other side: a key the
// store answers for, as unknown or as revoked, keeps its 401 and its
// bad_key count and logs nothing.
func TestUpgradeUnknownOrRevokedKeyIsStill401(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	revoked, _ := s.CreateKey(ctx, "revoked", auth.HashSecret(secret))
	if err := s.RevokeKey(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	h := NewUpgradeHandler(UpgradeDeps{Store: s})

	for name, keyID := range map[string]string{
		"unknown key": "01J8Z4Q0M3W6B9XKAV5T2C7RPD",
		"revoked key": revoked.ID,
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			badKey, storeErr := upgradeRejected(t, "bad_key"), upgradeRejected(t, "store_error")

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/connect?key="+keyID, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if d := upgradeRejected(t, "bad_key") - badKey; d != 1 {
				t.Errorf("bad_key counter moved by %v, want 1", d)
			}
			if d := upgradeRejected(t, "store_error") - storeErr; d != 0 {
				t.Errorf("store_error counter moved by %v, want 0", d)
			}
			if out := logs.String(); out != "" {
				t.Errorf("a bad key must not log; log output: %q", out)
			}
		})
	}
}

// TestUpgradeLookupCanceledByClientIsNotAStoreError: a lookup that fails
// only because the client went away mid-request (its context ended) is not
// a storage fault and not a bad key, so it moves neither counter and writes
// no error line.
func TestUpgradeLookupCanceledByClientIsNotAStoreError(t *testing.T) {
	logs := captureLogs(t)
	h := NewUpgradeHandler(UpgradeDeps{
		Store: failingLookupStore{store.NewMemory(), context.Canceled},
	})
	badKey, storeErr := upgradeRejected(t, "bad_key"), upgradeRejected(t, "store_error")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/v1/connect?key=01J8Z4Q0M3W6B9XKAV5T2C7RPD", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if d := upgradeRejected(t, "bad_key") - badKey; d != 0 {
		t.Errorf("bad_key counter moved by %v, want 0", d)
	}
	if d := upgradeRejected(t, "store_error") - storeErr; d != 0 {
		t.Errorf("store_error counter moved by %v, want 0", d)
	}
	if out := logs.String(); out != "" {
		t.Errorf("a client that went away must not log; log output: %q", out)
	}
}

// signWith posts a well-formed sign request with the given credentials to a
// RestHandler backed by s and returns the recorded response.
func signWith(s store.Store, ctx context.Context, creds string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	NewRestHandler(s, "admin-tok", "server-signing-secret", hub.New()).Register(mux)
	body := strings.NewReader(`{"socket_id":"` + testSocketID + `","channel":"private-room"}`)
	req := httptest.NewRequest("POST", "/v1/auth/sign", body).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+creds)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestAuthSignKeyStoreFailureIsNotBadCredentials is the /v1/auth/sign half:
// a store read failure used to be 401 "bad credentials" with nothing in the
// log, which tells an app server its secret is wrong when the fault is the
// store's.
func TestAuthSignKeyStoreFailureIsNotBadCredentials(t *testing.T) {
	logs := captureLogs(t)
	s := failingLookupStore{store.NewMemory(), errors.New("disk I/O error")}

	rec := signWith(s, context.Background(), "01J8Z4Q0M3W6B9XKAV5T2C7RPD:some-secret")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d %q, want %d", rec.Code, strings.TrimSpace(rec.Body.String()), http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "key store unavailable" {
		t.Errorf("body = %q, want %q", got, "key store unavailable")
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "disk I/O error") {
		t.Errorf("store failure was not logged as an error; log output: %q", out)
	}
}

// TestAuthSignBadCredentialsAreStill401: an unknown key, a revoked key and
// a wrong secret are answers from a working store and stay 401 with nothing
// logged.
func TestAuthSignBadCredentialsAreStill401(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	live, _ := s.CreateKey(ctx, "live", auth.HashSecret(secret))
	revoked, _ := s.CreateKey(ctx, "revoked", auth.HashSecret(secret))
	if err := s.RevokeKey(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}

	for name, creds := range map[string]string{
		"unknown key":  "01J8Z4Q0M3W6B9XKAV5T2C7RPD:" + secret,
		"revoked key":  revoked.ID + ":" + secret,
		"wrong secret": live.ID + ":not-the-secret",
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			rec := signWith(s, ctx, creds)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if out := logs.String(); out != "" {
				t.Errorf("bad credentials must not log; log output: %q", out)
			}
		})
	}
	if rec := signWith(s, ctx, live.ID+":"+secret); rec.Code != http.StatusOK {
		t.Fatalf("valid credentials: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestAuthSignLookupCanceledByClientIsNotLogged: as on the upgrade path, a
// lookup cut short by the client going away is not a store fault.
func TestAuthSignLookupCanceledByClientIsNotLogged(t *testing.T) {
	logs := captureLogs(t)
	s := failingLookupStore{store.NewMemory(), context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	signWith(s, ctx, "01J8Z4Q0M3W6B9XKAV5T2C7RPD:some-secret")

	if out := logs.String(); out != "" {
		t.Errorf("a client that went away must not log; log output: %q", out)
	}
}
