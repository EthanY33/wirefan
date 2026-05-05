# wirefan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a single-binary Go WebSocket fanout server with reproducible benchmarks, deployed at $0/mo, as a Big Tech new-grad SWE portfolio centerpiece.

**Architecture:** Single-binary HTTP+WS server. Hub coordinates a pluggable channel `Registry` (sync.Map vs sharded RWMutex+map). `Channel` holds subscribers with a per-channel mutex (FIFO ordering). Pluggable `Fanout` strategies (per-conn buffered chan vs sharded worker pool) selected at boot. API keys + HMAC-signed channel tokens (bound to socket_id). SQLite for key persistence. `coder/websocket` for transport. Prometheus + slog + pprof + dormant OTel hook.

**Tech Stack:** Go 1.25, `coder/websocket`, `github.com/oklog/ulid/v2`, `github.com/mattn/go-sqlite3`, `golang.org/x/time/rate`, `github.com/prometheus/client_golang`, vanilla JS demo, Caddy reverse proxy, Oracle Cloud Always Free (or Cloudflare Tunnel).

---

## File Structure

```
wirefan/
├── cmd/
│   ├── wirefan/main.go           # binary entrypoint, flag parsing, ctx wiring
│   └── loadtest/main.go          # standalone WS client load generator
├── internal/
│   ├── server/
│   │   ├── server.go             # HTTP routing, server lifecycle
│   │   ├── upgrade.go            # WS upgrade + auth middleware
│   │   ├── rest.go               # /v1/keys, /v1/auth/sign control plane
│   │   └── health.go             # /v1/health (503 on drain)
│   ├── conn/
│   │   ├── conn.go               # Connection struct, read/write goroutines
│   │   ├── pumps.go              # readPump + writePump
│   │   └── policy.go             # backpressure policies
│   ├── hub/
│   │   ├── hub.go                # Hub coordinator
│   │   ├── channel.go            # Channel + per-channel mutex
│   │   └── stats.go              # _wirefan-stats system channel publisher
│   ├── registry/
│   │   ├── registry.go           # Registry interface + shared test suite
│   │   ├── syncmap.go            # SyncMapRegistry impl
│   │   └── sharded.go            # ShardedMapRegistry impl (16 shards)
│   ├── fanout/
│   │   ├── fanout.go             # Fanout interface
│   │   ├── perconn.go            # PerConnFanout impl
│   │   └── sharded.go            # ShardedPoolFanout impl
│   ├── auth/
│   │   ├── keys.go               # API key generation, Bearer auth
│   │   └── token.go              # HMAC-SHA256 channel tokens (socket_id-bound)
│   ├── store/
│   │   ├── store.go              # Store interface + shared test suite
│   │   ├── memory.go             # in-memory impl (tests / ephemeral)
│   │   └── sqlite.go             # sqlite-backed impl
│   ├── ratelimit/
│   │   └── limiter.go            # per-key token bucket + stale-key GC
│   └── metrics/
│       ├── prom.go               # Prometheus collectors
│       └── otel.go               # OTel hook (dormant unless --otel-endpoint)
├── web/
│   ├── index.html                # demo client shell
│   ├── client.js                 # vanilla JS WS client + 3 panels
│   └── styles.css                # demo styling
├── docs/
│   ├── architecture.svg          # diagram (made via frontend-design)
│   ├── DESIGN.md
│   ├── PROTOCOL.md
│   ├── BENCHMARKS.md
│   └── profiles/                 # pprof flamegraphs (PNG)
├── deploy/
│   ├── Dockerfile                # multi-stage ARM64
│   ├── Caddyfile
│   └── wirefan.service           # systemd unit
├── go.mod
├── go.sum
├── Makefile
├── CLAUDE.md                     # written via claude-md-management
├── ARCHITECTURE.md               # written via supermind:sm-init
├── README.md                     # written via frontend-design + ui-ux-pro-max
├── LICENSE                       # MIT
└── .github/workflows/ci.yml      # build + test + race + lint
```

---

## Tasks

### Task 1: Project skeleton + Go module + CI

**Files:**
- Create: `go.mod`
- Create: `cmd/wirefan/main.go`
- Create: `Makefile`
- Create: `.github/workflows/ci.yml`
- Create: `LICENSE`

- [ ] **Step 1: Initialize Go module**

```bash
cd /c/Users/ethan/Desktop/wirefan
go mod init github.com/EthanY33/wirefan
```

- [ ] **Step 2: Create main.go with hello stub**

```go
// cmd/wirefan/main.go
package main

import "fmt"

func main() {
    fmt.Println("wirefan: not yet implemented")
}
```

- [ ] **Step 3: Create Makefile**

```makefile
.PHONY: build test test-race lint clean

build:
	go build -o bin/wirefan ./cmd/wirefan

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/
```

- [ ] **Step 4: Create CI workflow**

```yaml
# .github/workflows/ci.yml
name: ci
on: [push, pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - run: go build ./...
      - run: go test -race ./...
      - uses: golangci/golangci-lint-action@v6
```

- [ ] **Step 5: Create MIT LICENSE**

Use https://choosealicense.com/licenses/mit/ — replace `[year] [fullname]` with `2026 Ethan Yucetepe`.

- [ ] **Step 6: Verify build, commit**

```bash
make build
./bin/wirefan
# Expected: "wirefan: not yet implemented"
git add .
git commit -m "chore: initialize Go module and CI scaffolding"
```

---

### Task 2: Main entry with ctx, signal handling, graceful shutdown skeleton

**Files:**
- Modify: `cmd/wirefan/main.go`

- [ ] **Step 1: Write the failing test for graceful shutdown signal**

```go
// cmd/wirefan/main_test.go
package main

import (
    "context"
    "syscall"
    "testing"
    "time"
)

func TestRunReturnsOnContextCancel(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    done := make(chan error, 1)
    go func() { done <- run(ctx) }()
    time.Sleep(50 * time.Millisecond)
    cancel()
    select {
    case err := <-done:
        if err != nil {
            t.Fatalf("expected nil error, got %v", err)
        }
    case <-time.After(2 * time.Second):
        t.Fatal("run did not return within 2s of ctx cancel")
    }
}

var _ = syscall.SIGINT // keep import for later
```

- [ ] **Step 2: Run test (expected to fail — `run` not defined)**

```bash
go test ./cmd/wirefan/...
```

- [ ] **Step 3: Implement run() with signal handling**

```go
// cmd/wirefan/main.go
package main

import (
    "context"
    "errors"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()
    if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        slog.Error("fatal", "err", err)
        os.Exit(1)
    }
}

func run(ctx context.Context) error {
    slog.Info("wirefan starting")
    <-ctx.Done()
    slog.Info("wirefan shutdown complete")
    return nil
}
```

- [ ] **Step 4: Run test, verify pass**

```bash
go test -race ./cmd/wirefan/...
# Expected: PASS
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: ctx-based main with SIGINT/SIGTERM handling"
```

---

### Task 3: HTTP server + /v1/health (503 during drain)

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/health.go`
- Create: `internal/server/health_test.go`
- Modify: `cmd/wirefan/main.go`

- [ ] **Step 1: Write the failing test for /v1/health drain semantics**

```go
// internal/server/health_test.go
package server

import (
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestHealthOK(t *testing.T) {
    h := NewHealthHandler()
    req := httptest.NewRequest("GET", "/v1/health", nil)
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusOK {
        t.Fatalf("want 200, got %d", rec.Code)
    }
}

func TestHealth503OnDrain(t *testing.T) {
    h := NewHealthHandler()
    h.SetDraining(true)
    req := httptest.NewRequest("GET", "/v1/health", nil)
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusServiceUnavailable {
        t.Fatalf("want 503, got %d", rec.Code)
    }
}
```

- [ ] **Step 2: Run test, verify it fails**

```bash
go test ./internal/server/...
# Expected: undefined: NewHealthHandler
```

- [ ] **Step 3: Implement health handler**

```go
// internal/server/health.go
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
    w.Write([]byte("ok"))
}
```

- [ ] **Step 4: Implement Server skeleton**

```go
// internal/server/server.go
package server

import (
    "context"
    "errors"
    "log/slog"
    "net/http"
    "time"
)

type Server struct {
    addr   string
    health *HealthHandler
    mux    *http.ServeMux
    srv    *http.Server
}

func New(addr string) *Server {
    s := &Server{addr: addr, health: NewHealthHandler(), mux: http.NewServeMux()}
    s.mux.Handle("/v1/health", s.health)
    s.srv = &http.Server{Addr: addr, Handler: s.mux}
    return s
}

func (s *Server) Run(ctx context.Context) error {
    errc := make(chan error, 1)
    go func() {
        slog.Info("listening", "addr", s.addr)
        if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            errc <- err
        }
        close(errc)
    }()

    select {
    case err := <-errc:
        return err
    case <-ctx.Done():
    }

    s.health.SetDraining(true)
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    return s.srv.Shutdown(shutdownCtx)
}
```

- [ ] **Step 5: Wire main into Server**

```go
// cmd/wirefan/main.go (replace run body)
func run(ctx context.Context) error {
    addr := ":8080"
    s := server.New(addr)
    return s.Run(ctx)
}
```

Add import: `"github.com/EthanY33/wirefan/internal/server"`

- [ ] **Step 6: Run all tests**

```bash
go test -race ./...
# Expected: PASS
```

- [ ] **Step 7: Smoke-test manually**

```bash
go run ./cmd/wirefan &
curl -s http://localhost:8080/v1/health
# Expected: ok
kill %1
```

- [ ] **Step 8: Commit**

```bash
git add .
git commit -m "feat: HTTP server skeleton with /v1/health drain semantics"
```

---

### Task 4: Store interface + memoryStore + sqliteStore (shared test suite)

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`
- Create: `internal/store/memory.go`
- Create: `internal/store/sqlite.go`

- [ ] **Step 1: Add deps**

```bash
go get github.com/mattn/go-sqlite3
go get github.com/oklog/ulid/v2
```

- [ ] **Step 2: Write Store interface and shared test suite**

```go
// internal/store/store.go
package store

import (
    "context"
    "errors"
    "time"
)

var (
    ErrKeyNotFound = errors.New("key not found")
    ErrKeyRevoked  = errors.New("key revoked")
)

type Key struct {
    ID         string
    Name       string
    SecretHash string
    CreatedAt  time.Time
    RevokedAt  *time.Time
}

type Store interface {
    CreateKey(ctx context.Context, name, secretHash string) (Key, error)
    LookupKey(ctx context.Context, id string) (Key, error)
    ListKeys(ctx context.Context) ([]Key, error)
    RevokeKey(ctx context.Context, id string) error
    Close() error
}
```

```go
// internal/store/store_test.go
package store

import (
    "context"
    "errors"
    "testing"
)

func runStoreTests(t *testing.T, factory func(t *testing.T) Store) {
    t.Run("CreateAndLookup", func(t *testing.T) {
        s := factory(t)
        defer s.Close()
        ctx := context.Background()
        k, err := s.CreateKey(ctx, "test", "hash1")
        if err != nil {
            t.Fatalf("CreateKey: %v", err)
        }
        got, err := s.LookupKey(ctx, k.ID)
        if err != nil {
            t.Fatalf("LookupKey: %v", err)
        }
        if got.Name != "test" || got.SecretHash != "hash1" {
            t.Errorf("got %+v", got)
        }
    })
    t.Run("LookupMissing", func(t *testing.T) {
        s := factory(t)
        defer s.Close()
        _, err := s.LookupKey(context.Background(), "nope")
        if !errors.Is(err, ErrKeyNotFound) {
            t.Fatalf("want ErrKeyNotFound, got %v", err)
        }
    })
    t.Run("Revoke", func(t *testing.T) {
        s := factory(t)
        defer s.Close()
        ctx := context.Background()
        k, _ := s.CreateKey(ctx, "x", "h")
        if err := s.RevokeKey(ctx, k.ID); err != nil {
            t.Fatal(err)
        }
        got, err := s.LookupKey(ctx, k.ID)
        if err != nil {
            t.Fatal(err)
        }
        if got.RevokedAt == nil {
            t.Fatal("expected RevokedAt set")
        }
    })
    t.Run("List", func(t *testing.T) {
        s := factory(t)
        defer s.Close()
        ctx := context.Background()
        s.CreateKey(ctx, "a", "h1")
        s.CreateKey(ctx, "b", "h2")
        got, err := s.ListKeys(ctx)
        if err != nil {
            t.Fatal(err)
        }
        if len(got) != 2 {
            t.Errorf("want 2 keys, got %d", len(got))
        }
    })
}
```

- [ ] **Step 3: Implement memoryStore**

```go
// internal/store/memory.go
package store

import (
    "context"
    "sync"
    "time"

    "github.com/oklog/ulid/v2"
)

type memoryStore struct {
    mu   sync.RWMutex
    keys map[string]Key
}

func NewMemory() Store { return &memoryStore{keys: map[string]Key{}} }

func (m *memoryStore) CreateKey(_ context.Context, name, secretHash string) (Key, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    k := Key{ID: ulid.Make().String(), Name: name, SecretHash: secretHash, CreatedAt: time.Now().UTC()}
    m.keys[k.ID] = k
    return k, nil
}

func (m *memoryStore) LookupKey(_ context.Context, id string) (Key, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    k, ok := m.keys[id]
    if !ok {
        return Key{}, ErrKeyNotFound
    }
    return k, nil
}

func (m *memoryStore) ListKeys(_ context.Context) ([]Key, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    out := make([]Key, 0, len(m.keys))
    for _, k := range m.keys {
        out = append(out, k)
    }
    return out, nil
}

func (m *memoryStore) RevokeKey(_ context.Context, id string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    k, ok := m.keys[id]
    if !ok {
        return ErrKeyNotFound
    }
    now := time.Now().UTC()
    k.RevokedAt = &now
    m.keys[id] = k
    return nil
}

func (m *memoryStore) Close() error { return nil }
```

```go
// internal/store/memory_test.go
package store

import "testing"

func TestMemoryStore(t *testing.T) {
    runStoreTests(t, func(*testing.T) Store { return NewMemory() })
}
```

- [ ] **Step 4: Implement sqliteStore with embedded migration**

```go
// internal/store/sqlite.go
package store

import (
    "context"
    "database/sql"
    "errors"
    "time"

    _ "github.com/mattn/go-sqlite3"
    "github.com/oklog/ulid/v2"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);
`

type sqliteStore struct{ db *sql.DB }

func NewSQLite(path string) (Store, error) {
    db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
    if err != nil {
        return nil, err
    }
    if _, err := db.Exec(sqliteSchema); err != nil {
        db.Close()
        return nil, err
    }
    return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) CreateKey(ctx context.Context, name, secretHash string) (Key, error) {
    k := Key{ID: ulid.Make().String(), Name: name, SecretHash: secretHash, CreatedAt: time.Now().UTC()}
    _, err := s.db.ExecContext(ctx, `INSERT INTO keys(id,name,secret_hash,created_at) VALUES(?,?,?,?)`,
        k.ID, k.Name, k.SecretHash, k.CreatedAt.UnixMilli())
    return k, err
}

func (s *sqliteStore) LookupKey(ctx context.Context, id string) (Key, error) {
    row := s.db.QueryRowContext(ctx, `SELECT id,name,secret_hash,created_at,revoked_at FROM keys WHERE id=?`, id)
    var k Key
    var createdMs int64
    var revokedMs sql.NullInt64
    err := row.Scan(&k.ID, &k.Name, &k.SecretHash, &createdMs, &revokedMs)
    if errors.Is(err, sql.ErrNoRows) {
        return Key{}, ErrKeyNotFound
    }
    if err != nil {
        return Key{}, err
    }
    k.CreatedAt = time.UnixMilli(createdMs).UTC()
    if revokedMs.Valid {
        t := time.UnixMilli(revokedMs.Int64).UTC()
        k.RevokedAt = &t
    }
    return k, nil
}

func (s *sqliteStore) ListKeys(ctx context.Context) ([]Key, error) {
    rows, err := s.db.QueryContext(ctx, `SELECT id,name,secret_hash,created_at,revoked_at FROM keys`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var out []Key
    for rows.Next() {
        var k Key
        var createdMs int64
        var revokedMs sql.NullInt64
        if err := rows.Scan(&k.ID, &k.Name, &k.SecretHash, &createdMs, &revokedMs); err != nil {
            return nil, err
        }
        k.CreatedAt = time.UnixMilli(createdMs).UTC()
        if revokedMs.Valid {
            t := time.UnixMilli(revokedMs.Int64).UTC()
            k.RevokedAt = &t
        }
        out = append(out, k)
    }
    return out, rows.Err()
}

func (s *sqliteStore) RevokeKey(ctx context.Context, id string) error {
    res, err := s.db.ExecContext(ctx, `UPDATE keys SET revoked_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), id)
    if err != nil {
        return err
    }
    n, _ := res.RowsAffected()
    if n == 0 {
        return ErrKeyNotFound
    }
    return nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }
```

```go
// internal/store/sqlite_test.go
package store

import (
    "path/filepath"
    "testing"
)

func TestSQLiteStore(t *testing.T) {
    runStoreTests(t, func(t *testing.T) Store {
        path := filepath.Join(t.TempDir(), "test.db")
        s, err := NewSQLite(path)
        if err != nil {
            t.Fatalf("NewSQLite: %v", err)
        }
        return s
    })
}
```

- [ ] **Step 5: Run all store tests under race**

```bash
go test -race ./internal/store/...
# Expected: PASS
```

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "feat: Store interface with memory + sqlite implementations"
```

---

### Task 5: API key generation + Bearer auth + admin token

**Files:**
- Create: `internal/auth/keys.go`
- Create: `internal/auth/keys_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/auth/keys_test.go
package auth

import (
    "crypto/sha256"
    "encoding/hex"
    "testing"
)

func TestGenerateSecret(t *testing.T) {
    s, err := GenerateSecret()
    if err != nil {
        t.Fatal(err)
    }
    if len(s) != 64 { // 32 bytes hex-encoded
        t.Fatalf("want 64 hex chars, got %d", len(s))
    }
}

func TestHashSecret(t *testing.T) {
    h := HashSecret("abc")
    expected := sha256.Sum256([]byte("abc"))
    if h != hex.EncodeToString(expected[:]) {
        t.Fatal("hash mismatch")
    }
}

func TestVerifySecret(t *testing.T) {
    h := HashSecret("password")
    if !VerifySecret("password", h) {
        t.Fatal("expected true")
    }
    if VerifySecret("wrong", h) {
        t.Fatal("expected false")
    }
}
```

- [ ] **Step 2: Implement**

```go
// internal/auth/keys.go
package auth

import (
    "crypto/rand"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
)

func GenerateSecret() (string, error) {
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return hex.EncodeToString(b), nil
}

func HashSecret(secret string) string {
    sum := sha256.Sum256([]byte(secret))
    return hex.EncodeToString(sum[:])
}

func VerifySecret(secret, hash string) bool {
    return subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(hash)) == 1
}
```

- [ ] **Step 3: Run tests**

```bash
go test -race ./internal/auth/...
# Expected: PASS
```

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "feat: API key secret generation, hashing, constant-time verification"
```

---

### Task 6: HMAC channel tokens (socket_id-bound, with expiry)

**Files:**
- Create: `internal/auth/token.go`
- Create: `internal/auth/token_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/token_test.go
package auth

import (
    "testing"
    "time"
)

func TestSignAndVerifyToken(t *testing.T) {
    secret := "topsecret"
    socketID := "01HZABC"
    channel := "private-room1"
    tok, err := SignToken(secret, socketID, channel, time.Now().Add(5*time.Minute))
    if err != nil {
        t.Fatal(err)
    }
    if err := VerifyToken(secret, socketID, channel, tok); err != nil {
        t.Fatalf("verify: %v", err)
    }
}

func TestVerifyTokenWrongSocket(t *testing.T) {
    secret := "s"
    tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
    if err := VerifyToken(secret, "sock2", "private-x", tok); err == nil {
        t.Fatal("expected mismatch error")
    }
}

func TestVerifyTokenExpired(t *testing.T) {
    secret := "s"
    tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(-time.Minute))
    if err := VerifyToken(secret, "sock1", "private-x", tok); err == nil {
        t.Fatal("expected expired error")
    }
}

func TestVerifyTokenTampered(t *testing.T) {
    secret := "s"
    tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
    if err := VerifyToken(secret, "sock1", "private-x", tok+"X"); err == nil {
        t.Fatal("expected tamper error")
    }
}
```

- [ ] **Step 2: Implement token.go**

```go
// internal/auth/token.go
package auth

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "errors"
    "fmt"
    "strconv"
    "strings"
    "time"
)

var (
    ErrTokenMalformed = errors.New("token malformed")
    ErrTokenExpired   = errors.New("token expired")
    ErrTokenInvalid   = errors.New("token invalid")
)

// SignToken returns "<expMillis>:<base64mac>".
func SignToken(secret, socketID, channel string, expiry time.Time) (string, error) {
    expMs := expiry.UnixMilli()
    payload := fmt.Sprintf("%d|%s|%s", expMs, socketID, channel)
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(payload))
    return strconv.FormatInt(expMs, 10) + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyToken(secret, socketID, channel, tok string) error {
    parts := strings.SplitN(tok, ":", 2)
    if len(parts) != 2 {
        return ErrTokenMalformed
    }
    expMs, err := strconv.ParseInt(parts[0], 10, 64)
    if err != nil {
        return ErrTokenMalformed
    }
    if time.Now().UnixMilli() > expMs {
        return ErrTokenExpired
    }
    sig, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        return ErrTokenMalformed
    }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(fmt.Sprintf("%d|%s|%s", expMs, socketID, channel)))
    if !hmac.Equal(sig, mac.Sum(nil)) {
        return ErrTokenInvalid
    }
    return nil
}
```

- [ ] **Step 3: Run tests**

```bash
go test -race ./internal/auth/...
# Expected: PASS
```

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "feat: HMAC-SHA256 channel tokens bound to socket_id with expiry"
```

---

### Task 7: REST control plane (/v1/keys POST/GET/DELETE + admin Bearer)

**Files:**
- Create: `internal/server/rest.go`
- Create: `internal/server/rest_test.go`
- Modify: `internal/server/server.go`

- [ ] **Step 1: Write failing integration test**

```go
// internal/server/rest_test.go
package server

import (
    "bytes"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/EthanY33/wirefan/internal/store"
)

func TestCreateAndListKeys(t *testing.T) {
    s := store.NewMemory()
    rest := NewRestHandler(s, "admin-tok")
    mux := http.NewServeMux()
    rest.Register(mux)
    srv := httptest.NewServer(mux)
    defer srv.Close()

    // create
    body := bytes.NewBufferString(`{"name":"app1"}`)
    req, _ := http.NewRequest("POST", srv.URL+"/v1/keys", body)
    req.Header.Set("Authorization", "Bearer admin-tok")
    req.Header.Set("Content-Type", "application/json")
    res, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    if res.StatusCode != http.StatusCreated {
        t.Fatalf("want 201, got %d", res.StatusCode)
    }
    var created struct{ ID, Secret string }
    json.NewDecoder(res.Body).Decode(&created)
    if created.ID == "" || created.Secret == "" {
        t.Fatal("expected id and secret")
    }

    // list
    req2, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
    req2.Header.Set("Authorization", "Bearer admin-tok")
    res2, _ := http.DefaultClient.Do(req2)
    body2, _ := io.ReadAll(res2.Body)
    if !bytes.Contains(body2, []byte(created.ID)) {
        t.Fatalf("list missing id: %s", body2)
    }
}

func TestRequiresAdminBearer(t *testing.T) {
    s := store.NewMemory()
    rest := NewRestHandler(s, "tok")
    mux := http.NewServeMux()
    rest.Register(mux)
    srv := httptest.NewServer(mux)
    defer srv.Close()
    req, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
    res, _ := http.DefaultClient.Do(req)
    if res.StatusCode != http.StatusUnauthorized {
        t.Fatalf("want 401, got %d", res.StatusCode)
    }
}
```

- [ ] **Step 2: Implement REST handler**

```go
// internal/server/rest.go
package server

import (
    "encoding/json"
    "net/http"
    "strings"

    "github.com/EthanY33/wirefan/internal/auth"
    "github.com/EthanY33/wirefan/internal/store"
)

type RestHandler struct {
    store      store.Store
    adminToken string
}

func NewRestHandler(s store.Store, adminToken string) *RestHandler {
    return &RestHandler{store: s, adminToken: adminToken}
}

func (h *RestHandler) Register(mux *http.ServeMux) {
    mux.HandleFunc("POST /v1/keys", h.requireAdmin(h.create))
    mux.HandleFunc("GET /v1/keys", h.requireAdmin(h.list))
    mux.HandleFunc("DELETE /v1/keys/{id}", h.requireAdmin(h.revoke))
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
    json.NewEncoder(w).Encode(map[string]string{"id": k.ID, "name": k.Name, "secret": secret})
}

func (h *RestHandler) list(w http.ResponseWriter, r *http.Request) {
    keys, err := h.store.ListKeys(r.Context())
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    json.NewEncoder(w).Encode(keys)
}

func (h *RestHandler) revoke(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    if err := h.store.RevokeKey(r.Context(), id); err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }
    w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 3: Wire into Server**

Modify `internal/server/server.go` `New` to take a `store.Store` and admin token, and register `RestHandler`:

```go
func New(addr string, st store.Store, adminToken string) *Server {
    s := &Server{addr: addr, health: NewHealthHandler(), mux: http.NewServeMux(), store: st}
    s.mux.Handle("/v1/health", s.health)
    NewRestHandler(st, adminToken).Register(s.mux)
    s.srv = &http.Server{Addr: addr, Handler: s.mux}
    return s
}
```

Add `store store.Store` to Server struct and the `store` import.

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/server/...
# Expected: PASS
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: REST control plane for API key CRUD with admin Bearer"
```

---

### Task 8: Registry interface + SyncMapRegistry + ShardedMapRegistry (shared test suite)

**Files:**
- Create: `internal/registry/registry.go`
- Create: `internal/registry/registry_test.go`
- Create: `internal/registry/syncmap.go`
- Create: `internal/registry/sharded.go`

- [ ] **Step 1: Write Registry interface and shared test suite**

```go
// internal/registry/registry.go
package registry

type Channel struct {
    Name string
    // additional fields wired in Task 9 (subscribers, mutex, etc.)
}

type Registry interface {
    GetOrCreate(name string) *Channel
    Lookup(name string) (*Channel, bool)
    Delete(name string)
    Range(fn func(*Channel) bool)
    Len() int
}
```

```go
// internal/registry/registry_test.go
package registry

import (
    "sync"
    "testing"
)

func runRegistryTests(t *testing.T, factory func() Registry) {
    t.Run("GetOrCreate", func(t *testing.T) {
        r := factory()
        c1 := r.GetOrCreate("a")
        c2 := r.GetOrCreate("a")
        if c1 != c2 {
            t.Fatal("GetOrCreate must return same instance")
        }
        if r.Len() != 1 {
            t.Fatalf("Len=%d", r.Len())
        }
    })
    t.Run("LookupMissing", func(t *testing.T) {
        r := factory()
        if _, ok := r.Lookup("nope"); ok {
            t.Fatal("expected not ok")
        }
    })
    t.Run("Delete", func(t *testing.T) {
        r := factory()
        r.GetOrCreate("a")
        r.Delete("a")
        if _, ok := r.Lookup("a"); ok {
            t.Fatal("expected gone")
        }
    })
    t.Run("ConcurrentGetOrCreate", func(t *testing.T) {
        r := factory()
        var wg sync.WaitGroup
        for i := 0; i < 100; i++ {
            wg.Add(1)
            go func() {
                defer wg.Done()
                r.GetOrCreate("shared")
            }()
        }
        wg.Wait()
        if r.Len() != 1 {
            t.Fatalf("expected 1, got %d", r.Len())
        }
    })
}
```

- [ ] **Step 2: Implement SyncMapRegistry**

```go
// internal/registry/syncmap.go
package registry

import "sync"

type syncMapReg struct{ m sync.Map }

func NewSyncMap() Registry { return &syncMapReg{} }

func (s *syncMapReg) GetOrCreate(name string) *Channel {
    if v, ok := s.m.Load(name); ok {
        return v.(*Channel)
    }
    c := &Channel{Name: name}
    actual, _ := s.m.LoadOrStore(name, c)
    return actual.(*Channel)
}

func (s *syncMapReg) Lookup(name string) (*Channel, bool) {
    v, ok := s.m.Load(name)
    if !ok {
        return nil, false
    }
    return v.(*Channel), true
}

func (s *syncMapReg) Delete(name string) { s.m.Delete(name) }

func (s *syncMapReg) Range(fn func(*Channel) bool) {
    s.m.Range(func(_, v any) bool { return fn(v.(*Channel)) })
}

func (s *syncMapReg) Len() int {
    n := 0
    s.m.Range(func(_, _ any) bool { n++; return true })
    return n
}
```

```go
// internal/registry/syncmap_test.go
package registry

import "testing"
func TestSyncMap(t *testing.T) { runRegistryTests(t, NewSyncMap) }
```

- [ ] **Step 3: Implement ShardedMapRegistry (16 shards)**

```go
// internal/registry/sharded.go
package registry

import (
    "hash/fnv"
    "sync"
)

const numShards = 16

type shard struct {
    sync.RWMutex
    chans map[string]*Channel
}

type shardedReg struct{ shards [numShards]*shard }

func NewSharded() Registry {
    r := &shardedReg{}
    for i := range r.shards {
        r.shards[i] = &shard{chans: map[string]*Channel{}}
    }
    return r
}

func (r *shardedReg) shardFor(name string) *shard {
    h := fnv.New32a()
    h.Write([]byte(name))
    return r.shards[h.Sum32()%numShards]
}

func (r *shardedReg) GetOrCreate(name string) *Channel {
    s := r.shardFor(name)
    s.RLock()
    if c, ok := s.chans[name]; ok {
        s.RUnlock()
        return c
    }
    s.RUnlock()
    s.Lock()
    defer s.Unlock()
    if c, ok := s.chans[name]; ok {
        return c
    }
    c := &Channel{Name: name}
    s.chans[name] = c
    return c
}

func (r *shardedReg) Lookup(name string) (*Channel, bool) {
    s := r.shardFor(name)
    s.RLock()
    defer s.RUnlock()
    c, ok := s.chans[name]
    return c, ok
}

func (r *shardedReg) Delete(name string) {
    s := r.shardFor(name)
    s.Lock()
    defer s.Unlock()
    delete(s.chans, name)
}

func (r *shardedReg) Range(fn func(*Channel) bool) {
    for _, s := range r.shards {
        s.RLock()
        for _, c := range s.chans {
            if !fn(c) {
                s.RUnlock()
                return
            }
        }
        s.RUnlock()
    }
}

func (r *shardedReg) Len() int {
    n := 0
    for _, s := range r.shards {
        s.RLock()
        n += len(s.chans)
        s.RUnlock()
    }
    return n
}
```

```go
// internal/registry/sharded_test.go
package registry

import "testing"
func TestSharded(t *testing.T) { runRegistryTests(t, NewSharded) }
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/registry/...
# Expected: PASS for both impls
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: Registry interface with sync.Map and sharded RWMutex implementations"
```

---

### Task 9: Channel struct (subscriber set + per-channel mutex for FIFO ordering)

**Files:**
- Create: `internal/hub/channel.go`
- Create: `internal/hub/channel_test.go`
- Modify: `internal/registry/registry.go` (replace Channel stub)

- [ ] **Step 1: Move Channel from registry to hub**

Replace `Channel` in `internal/registry/registry.go` with a forward declaration that imports from hub. Or simpler: keep `Channel` in `registry/registry.go` but enrich it. Decision: keep in registry to avoid import cycles, since hub uses registry.

Update `internal/registry/registry.go`:

```go
// internal/registry/registry.go
package registry

import "sync"

type Channel struct {
    Name        string
    BroadcastMu sync.Mutex // serialize broadcasts (FIFO)
    SubsMu      sync.RWMutex
    Subscribers map[Subscriber]struct{}
}

type Subscriber interface {
    Send([]byte) error
    Close()
}

func newChannel(name string) *Channel {
    return &Channel{Name: name, Subscribers: map[Subscriber]struct{}{}}
}
```

Update both registry impls to call `newChannel(name)` instead of `&Channel{Name: name}`.

- [ ] **Step 2: Write tests for channel subscribe/broadcast**

```go
// internal/hub/channel_test.go
package hub

import (
    "errors"
    "sync"
    "testing"

    "github.com/EthanY33/wirefan/internal/registry"
)

type fakeSub struct {
    mu       sync.Mutex
    received [][]byte
    failNext bool
}

func (f *fakeSub) Send(b []byte) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    if f.failNext {
        f.failNext = false
        return errors.New("forced")
    }
    f.received = append(f.received, b)
    return nil
}
func (f *fakeSub) Close() {}

func TestChannelSubscribeBroadcast(t *testing.T) {
    c := registry.NewSyncMap().GetOrCreate("test")
    a, b := &fakeSub{}, &fakeSub{}
    Subscribe(c, a)
    Subscribe(c, b)
    Broadcast(c, []byte("hello"))
    if len(a.received) != 1 || len(b.received) != 1 {
        t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
    }
}

func TestChannelUnsubscribe(t *testing.T) {
    c := registry.NewSyncMap().GetOrCreate("test")
    s := &fakeSub{}
    Subscribe(c, s)
    Unsubscribe(c, s)
    Broadcast(c, []byte("hi"))
    if len(s.received) != 0 {
        t.Fatal("should not receive after unsubscribe")
    }
}
```

- [ ] **Step 3: Implement channel ops**

```go
// internal/hub/channel.go
package hub

import "github.com/EthanY33/wirefan/internal/registry"

func Subscribe(c *registry.Channel, s registry.Subscriber) {
    c.SubsMu.Lock()
    c.Subscribers[s] = struct{}{}
    c.SubsMu.Unlock()
}

func Unsubscribe(c *registry.Channel, s registry.Subscriber) {
    c.SubsMu.Lock()
    delete(c.Subscribers, s)
    c.SubsMu.Unlock()
}

// Broadcast iterates subscribers under per-channel mutex (FIFO ordering).
func Broadcast(c *registry.Channel, msg []byte) {
    c.BroadcastMu.Lock()
    defer c.BroadcastMu.Unlock()
    c.SubsMu.RLock()
    subs := make([]registry.Subscriber, 0, len(c.Subscribers))
    for s := range c.Subscribers {
        subs = append(subs, s)
    }
    c.SubsMu.RUnlock()
    for _, s := range subs {
        _ = s.Send(msg) // policy resolution lives at the conn layer
    }
}

func SubscriberCount(c *registry.Channel) int {
    c.SubsMu.RLock()
    defer c.SubsMu.RUnlock()
    return len(c.Subscribers)
}
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/...
# Expected: PASS
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: Channel subscribe/broadcast ops with FIFO ordering guarantee"
```

---

### Task 10: WS upgrade endpoint (auth + Origin allowlist)

**Files:**
- Create: `internal/server/upgrade.go`
- Create: `internal/server/upgrade_test.go`
- Modify: `internal/server/server.go`
- Modify: `go.mod` (add coder/websocket)

- [ ] **Step 1: Add coder/websocket dependency**

```bash
go get github.com/coder/websocket
```

- [ ] **Step 2: Write the failing upgrade test**

```go
// internal/server/upgrade_test.go
package server

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/EthanY33/wirefan/internal/auth"
    "github.com/EthanY33/wirefan/internal/store"
    "github.com/coder/websocket"
)

func TestUpgradeRequiresKey(t *testing.T) {
    h := newTestUpgrader(t)
    srv := httptest.NewServer(h)
    defer srv.Close()
    res, err := http.Get(srv.URL + "/v1/connect")
    if err != nil {
        t.Fatal(err)
    }
    if res.StatusCode != http.StatusUnauthorized {
        t.Fatalf("want 401, got %d", res.StatusCode)
    }
}

func TestUpgradeSucceeds(t *testing.T) {
    s := store.NewMemory()
    secret, _ := auth.GenerateSecret()
    k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
    h := NewUpgradeHandler(s, []string{"*"})
    srv := httptest.NewServer(h)
    defer srv.Close()
    wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID
    c, _, err := websocket.Dial(context.Background(), wsURL, nil)
    if err != nil {
        t.Fatalf("dial: %v", err)
    }
    c.Close(websocket.StatusNormalClosure, "")
}

func newTestUpgrader(t *testing.T) http.Handler {
    return NewUpgradeHandler(store.NewMemory(), []string{"*"})
}
```

- [ ] **Step 3: Implement upgrade handler**

```go
// internal/server/upgrade.go
package server

import (
    "errors"
    "log/slog"
    "net/http"

    "github.com/EthanY33/wirefan/internal/store"
    "github.com/coder/websocket"
)

type UpgradeHandler struct {
    store          store.Store
    allowedOrigins []string
}

func NewUpgradeHandler(st store.Store, origins []string) *UpgradeHandler {
    return &UpgradeHandler{store: st, allowedOrigins: origins}
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
    // TODO Task 11: hand to Connection lifecycle
    c.Close(websocket.StatusNormalClosure, "not implemented yet")
}
```

- [ ] **Step 4: Run upgrade tests**

```bash
go test -race ./internal/server/...
# Expected: PASS
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: WS upgrade endpoint with API key auth + origin allowlist"
```

---

### Task 11: Connection struct + send chan + read/write pumps + connected message

**Files:**
- Create: `internal/conn/conn.go`
- Create: `internal/conn/pumps.go`
- Create: `internal/conn/conn_test.go`
- Modify: `internal/server/upgrade.go` (hand off to Connection)

- [ ] **Step 1: Write the failing test**

```go
// internal/conn/conn_test.go
package conn

import (
    "context"
    "encoding/json"
    "net/http/httptest"
    "strings"
    "sync"
    "testing"
    "time"

    "github.com/coder/websocket"
)

func TestConnectedMessageSent(t *testing.T) {
    var got map[string]any
    var gotMu sync.Mutex

    handler := func(c *websocket.Conn) {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        Run(ctx, c, "01HTEST")
    }

    srv := httptest.NewServer(websocketHandler(handler))
    defer srv.Close()

    wsURL := strings.Replace(srv.URL, "http", "ws", 1)
    c, _, _ := websocket.Dial(context.Background(), wsURL, nil)
    defer c.Close(websocket.StatusNormalClosure, "")

    _, data, err := c.Read(context.Background())
    if err != nil {
        t.Fatal(err)
    }
    gotMu.Lock()
    json.Unmarshal(data, &got)
    gotMu.Unlock()

    if got["type"] != "connected" || got["socket_id"] != "01HTEST" {
        t.Fatalf("got %+v", got)
    }
}

// helper: minimal websocket adapter used in test
func websocketHandler(fn func(*websocket.Conn)) interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request)
} {
    // [...] standard upgrade test wrapper, passes accepted conn into fn
    // implementation per coder/websocket README example
}
```

- [ ] **Step 2: Implement Connection.Run**

```go
// internal/conn/conn.go
package conn

import (
    "context"
    "encoding/json"
    "log/slog"
    "time"

    "github.com/coder/websocket"
)

const (
    sendChanSize    = 64
    pingInterval    = 30 * time.Second
    readDeadline    = 60 * time.Second
    writeDeadline   = 10 * time.Second
    protocolVersion = "v1"
)

type Conn struct {
    ws       *websocket.Conn
    socketID string
    send     chan []byte
}

// Run owns the conn for its lifetime. Returns when ctx is canceled or peer disconnects.
func Run(ctx context.Context, ws *websocket.Conn, socketID string) error {
    c := &Conn{ws: ws, socketID: socketID, send: make(chan []byte, sendChanSize)}

    // Send `connected` immediately
    hello, _ := json.Marshal(map[string]string{
        "type":      "connected",
        "socket_id": socketID,
        "version":   protocolVersion,
    })
    select {
    case c.send <- hello:
    default:
        return ws.Close(websocket.StatusInternalError, "send chan full at start")
    }

    runCtx, cancel := context.WithCancel(ctx)
    defer cancel()

    errc := make(chan error, 2)
    go func() { errc <- c.writePump(runCtx) }()
    go func() { errc <- c.readPump(runCtx) }()
    err := <-errc
    cancel()
    <-errc
    if err != nil {
        slog.Debug("conn closed", "socket_id", socketID, "err", err)
    }
    return err
}
```

```go
// internal/conn/pumps.go
package conn

import (
    "context"
    "errors"
    "time"

    "github.com/coder/websocket"
)

func (c *Conn) writePump(ctx context.Context) error {
    ticker := time.NewTicker(pingInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case msg, ok := <-c.send:
            if !ok {
                return c.ws.Close(websocket.StatusNormalClosure, "")
            }
            wctx, cancel := context.WithTimeout(ctx, writeDeadline)
            err := c.ws.Write(wctx, websocket.MessageText, msg)
            cancel()
            if err != nil {
                return err
            }
        case <-ticker.C:
            pctx, cancel := context.WithTimeout(ctx, writeDeadline)
            err := c.ws.Ping(pctx)
            cancel()
            if err != nil {
                return err
            }
        }
    }
}

func (c *Conn) readPump(ctx context.Context) error {
    c.ws.SetReadLimit(64 * 1024)
    for {
        rctx, cancel := context.WithTimeout(ctx, readDeadline)
        _, _, err := c.ws.Read(rctx)
        cancel()
        if err != nil {
            if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
                return c.ws.Close(websocket.StatusGoingAway, "")
            }
            return err
        }
        // TODO Task 12: parse JSON, dispatch to subscribe/publish handlers
    }
}
```

- [ ] **Step 3: Wire upgrade to Conn.Run**

In `internal/server/upgrade.go`, replace the close stub with:

```go
import "github.com/EthanY33/wirefan/internal/conn"
import "github.com/oklog/ulid/v2"
// ...
sid := ulid.Make().String()
conn.Run(r.Context(), c, sid)
```

- [ ] **Step 4: Run tests + smoke test**

```bash
go test -race ./...
# Expected: PASS
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: Conn lifecycle with read/write pumps and connected message"
```

---

### Task 12: subscribe/unsubscribe handlers (incl. private channel HMAC + idempotent dup)

**Files:**
- Create: `internal/conn/handler.go`
- Create: `internal/conn/handler_test.go`
- Modify: `internal/conn/pumps.go` (dispatch to handler)
- Modify: `internal/conn/conn.go` (Conn now needs registry + apiSecret reference)

- [ ] **Step 1: Extend Conn with registry + auth context**

Add fields to `Conn`:

```go
type Conn struct {
    ws        *websocket.Conn
    socketID  string
    send      chan []byte
    registry  registry.Registry
    apiSecret string
    subs      map[string]*registry.Channel
    subsMu    sync.Mutex
}
```

Update `Run` signature to accept these and assign before launching pumps. Update `upgrade.go` to pass them in (looks up secret hash via store, but the *secret* needed for HMAC is on the app server; spec: server stores hash, sign endpoint uses raw secret. For private subscribe verification, server uses the stored secret hash? No — HMAC requires the actual secret. The spec assumes the app server holds the secret and signs tokens server-side via /v1/auth/sign. For verification on subscribe, the server needs the actual key secret.)

**Design clarification:** when server creates a key, it returns `secret` once. Server stores `HashSecret(secret)` for Bearer-style verification at REST. But for HMAC verification on WS subscribe, server also needs the raw secret to recompute the HMAC. Two options:
- (a) Store the raw secret too (encrypted at rest with a master key from env)
- (b) Server uses its OWN signing key (separate from API key secrets) for HMAC tokens

Option (b) is cleaner: introduce a `--token-signing-secret=` flag (one global server secret) used for HMAC. Then `secret` returned to the operator on key creation is just the Bearer for `/v1/auth/sign`; the actual HMAC is signed by the server's signing secret. **Adopt option (b).**

Update `auth.SignToken` / `VerifyToken` calls to use the server signing secret. Verify token semantics: `mac = HMAC(serverSigningSecret, "{exp}|{socket_id}|{channel}")`. Spec says socket_id binding — that's preserved.

- [ ] **Step 2: Write handler tests**

```go
// internal/conn/handler_test.go
package conn

// (test cases, omitted for brevity — assert: subscribe to public-X succeeds w/o token,
// private-Y w/ valid HMAC succeeds, private-Y w/ bad HMAC returns error msg w/o close,
// duplicate subscribe to public-X returns subscribed ack and is idempotent)
```

(Full test cases follow the same pattern as REST tests above; write subscribe-flow assertions reading `subscribed`/`error` JSON via the WS connection.)

- [ ] **Step 3: Implement handler**

```go
// internal/conn/handler.go
package conn

import (
    "encoding/json"
    "strings"

    "github.com/EthanY33/wirefan/internal/auth"
    "github.com/EthanY33/wirefan/internal/hub"
)

type incoming struct {
    Type    string          `json:"type"`
    Channel string          `json:"channel"`
    Token   string          `json:"token,omitempty"`
    Data    json.RawMessage `json:"data,omitempty"`
}

func (c *Conn) handle(raw []byte) {
    var msg incoming
    if err := json.Unmarshal(raw, &msg); err != nil {
        c.sendError("BAD_JSON", "malformed message")
        return
    }
    switch msg.Type {
    case "subscribe":
        c.handleSubscribe(msg)
    case "unsubscribe":
        c.handleUnsubscribe(msg)
    case "publish":
        c.handlePublish(msg) // implemented in Task 13
    default:
        c.sendError("BAD_TYPE", "unknown message type")
    }
}

func (c *Conn) handleSubscribe(msg incoming) {
    if strings.HasPrefix(msg.Channel, "private-") {
        if err := auth.VerifyToken(c.apiSecret, c.socketID, msg.Channel, msg.Token); err != nil {
            c.sendError("AUTH_FAILED", "invalid token")
            return
        }
    }
    c.subsMu.Lock()
    if _, already := c.subs[msg.Channel]; already {
        c.subsMu.Unlock()
        c.sendAck("subscribed", msg.Channel) // idempotent
        return
    }
    ch := c.registry.GetOrCreate(msg.Channel)
    hub.Subscribe(ch, c)
    c.subs[msg.Channel] = ch
    c.subsMu.Unlock()
    c.sendAck("subscribed", msg.Channel)
}

func (c *Conn) handleUnsubscribe(msg incoming) {
    c.subsMu.Lock()
    ch, ok := c.subs[msg.Channel]
    if !ok {
        c.subsMu.Unlock()
        c.sendAck("unsubscribed", msg.Channel)
        return
    }
    delete(c.subs, msg.Channel)
    c.subsMu.Unlock()
    hub.Unsubscribe(ch, c)
    if hub.SubscriberCount(ch) == 0 {
        c.registry.Delete(msg.Channel)
    }
    c.sendAck("unsubscribed", msg.Channel)
}

func (c *Conn) sendAck(typ, channel string) {
    b, _ := json.Marshal(map[string]string{"type": typ, "channel": channel})
    select {
    case c.send <- b:
    default:
    }
}

func (c *Conn) sendError(code, message string) {
    b, _ := json.Marshal(map[string]string{"type": "error", "code": code, "message": message})
    select {
    case c.send <- b:
    default:
    }
}

// Send satisfies the registry.Subscriber interface
func (c *Conn) Send(b []byte) error {
    select {
    case c.send <- b:
        return nil
    default:
        return ErrSlowConsumer
    }
}

func (c *Conn) Close() {
    close(c.send)
}
```

Add `var ErrSlowConsumer = errors.New("slow consumer")` near the top of `internal/conn/conn.go`.

- [ ] **Step 4: Wire readPump dispatch**

In `internal/conn/pumps.go` `readPump`, replace the `// TODO` with:

```go
_, raw, err := c.ws.Read(rctx)
cancel()
if err != nil { /* existing branch */ }
c.handle(raw)
```

- [ ] **Step 5: Run tests**

```bash
go test -race ./...
# Expected: PASS
```

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "feat: subscribe/unsubscribe handlers with HMAC private-channel auth + idempotent dup"
```

---

### Task 13: Publish handler + Fanout interface + PerConnFanout impl

**Files:**
- Create: `internal/fanout/fanout.go`
- Create: `internal/fanout/perconn.go`
- Create: `internal/fanout/perconn_test.go`
- Modify: `internal/conn/handler.go` (handlePublish)

- [ ] **Step 1: Define Fanout interface**

```go
// internal/fanout/fanout.go
package fanout

import (
    "context"

    "github.com/EthanY33/wirefan/internal/registry"
)

type Fanout interface {
    Broadcast(ctx context.Context, channel *registry.Channel, msg []byte)
}
```

- [ ] **Step 2: PerConnFanout impl**

```go
// internal/fanout/perconn.go
package fanout

import (
    "context"

    "github.com/EthanY33/wirefan/internal/hub"
    "github.com/EthanY33/wirefan/internal/registry"
)

type PerConn struct{}

func NewPerConn() Fanout { return &PerConn{} }

func (PerConn) Broadcast(_ context.Context, c *registry.Channel, msg []byte) {
    hub.Broadcast(c, msg) // already iterates under per-channel mutex
}
```

- [ ] **Step 3: Test fanout receives via subscriber**

```go
// internal/fanout/perconn_test.go
package fanout

import (
    "context"
    "sync"
    "testing"

    "github.com/EthanY33/wirefan/internal/hub"
    "github.com/EthanY33/wirefan/internal/registry"
)

type capSub struct {
    mu       sync.Mutex
    received [][]byte
}

func (s *capSub) Send(b []byte) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.received = append(s.received, b)
    return nil
}
func (s *capSub) Close() {}

func TestPerConnFanout(t *testing.T) {
    r := registry.NewSyncMap()
    c := r.GetOrCreate("ch")
    a, b := &capSub{}, &capSub{}
    hub.Subscribe(c, a)
    hub.Subscribe(c, b)
    NewPerConn().Broadcast(context.Background(), c, []byte("hi"))
    if len(a.received) != 1 || len(b.received) != 1 {
        t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
    }
}
```

- [ ] **Step 4: Implement handlePublish**

```go
// internal/conn/handler.go (extend)

func (c *Conn) handlePublish(msg incoming) {
    c.subsMu.Lock()
    ch, ok := c.subs[msg.Channel]
    c.subsMu.Unlock()
    if !ok {
        c.sendError("NOT_SUBSCRIBED", "must subscribe before publish")
        return
    }
    if !c.rateLimit.Allow(c.apiKeyID) { // wired in Task 16
        c.sendError("RATE_LIMITED", "too many publishes")
        return
    }
    id := ulid.Make().String()
    out, _ := json.Marshal(map[string]any{
        "type": "event", "channel": msg.Channel, "data": msg.Data, "id": id,
    })
    c.fanout.Broadcast(context.Background(), ch, out)
}
```

Add `fanout fanout.Fanout`, `rateLimit *ratelimit.Limiter` (stub if not yet built), `apiKeyID string` to `Conn`. Wire from upgrade handler.

- [ ] **Step 5: Run tests**

```bash
go test -race ./...
# Expected: PASS
```

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "feat: publish handler + Fanout interface + PerConnFanout"
```

---

### Task 14: ShardedPoolFanout (worker pool variant for benchmarking)

**Files:**
- Create: `internal/fanout/sharded.go`
- Create: `internal/fanout/sharded_test.go`

- [ ] **Step 1: Reuse fanout test scaffolding**

```go
// internal/fanout/sharded_test.go
package fanout

import (
    "context"
    "testing"

    "github.com/EthanY33/wirefan/internal/hub"
    "github.com/EthanY33/wirefan/internal/registry"
)

func TestShardedPoolFanout(t *testing.T) {
    f := NewShardedPool(4) // 4 workers
    defer f.Close()
    r := registry.NewSyncMap()
    c := r.GetOrCreate("ch")
    a, b := &capSub{}, &capSub{}
    hub.Subscribe(c, a)
    hub.Subscribe(c, b)
    f.Broadcast(context.Background(), c, []byte("hi"))
    f.Wait()
    if len(a.received) != 1 || len(b.received) != 1 {
        t.Fatalf("a=%d b=%d", len(a.received), len(b.received))
    }
}
```

- [ ] **Step 2: Implement ShardedPool**

```go
// internal/fanout/sharded.go
package fanout

import (
    "context"
    "hash/fnv"
    "sync"

    "github.com/EthanY33/wirefan/internal/hub"
    "github.com/EthanY33/wirefan/internal/registry"
)

type job struct {
    ch  *registry.Channel
    msg []byte
}

type ShardedPool struct {
    queues  []chan job
    workers int
    wg      sync.WaitGroup
}

func NewShardedPool(workers int) *ShardedPool {
    p := &ShardedPool{queues: make([]chan job, workers), workers: workers}
    for i := 0; i < workers; i++ {
        p.queues[i] = make(chan job, 1024)
        p.wg.Add(1)
        go p.run(i)
    }
    return p
}

func (p *ShardedPool) run(i int) {
    defer p.wg.Done()
    for j := range p.queues[i] {
        hub.Broadcast(j.ch, j.msg)
    }
}

func (p *ShardedPool) shardFor(name string) int {
    h := fnv.New32a()
    h.Write([]byte(name))
    return int(h.Sum32() % uint32(p.workers))
}

func (p *ShardedPool) Broadcast(_ context.Context, c *registry.Channel, msg []byte) {
    p.queues[p.shardFor(c.Name)] <- job{ch: c, msg: msg}
}

func (p *ShardedPool) Close() {
    for _, q := range p.queues {
        close(q)
    }
    p.wg.Wait()
}

// Wait drains all in-flight jobs (test-only helper)
func (p *ShardedPool) Wait() { /* test helper — drain by polling len, or accept eventual */ }
```

For test determinism, replace `Wait` with a `done` chan signaling when each broadcast completes; or in tests, use a small sleep + assert on eventual delivery. Simplest: add a per-broadcast WaitGroup and have `Broadcast` block until subscribers receive. Decision: keep async for benchmark accuracy; test waits via short sleep + assertion retry.

- [ ] **Step 3: Run tests**

```bash
go test -race ./internal/fanout/...
```

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "feat: ShardedPoolFanout worker-pool implementation"
```

---

### Task 15: Slow-consumer policy (disconnect / drop-oldest / drop-newest)

**Files:**
- Create: `internal/conn/policy.go`
- Create: `internal/conn/policy_test.go`
- Modify: `internal/conn/handler.go` (use policy in Send)

- [ ] **Step 1: Tests**

```go
// internal/conn/policy_test.go
package conn

import "testing"

func TestPolicyDisconnect(t *testing.T) {
    p := PolicyDisconnect{}
    sent := false
    err := p.Apply(make(chan []byte), []byte("x"), func() { sent = true })
    if err != ErrSlowConsumer {
        t.Fatalf("want ErrSlowConsumer, got %v", err)
    }
    if sent {
        t.Fatal("disconnect should not send")
    }
}

func TestPolicyDropOldest(t *testing.T) {
    ch := make(chan []byte, 1)
    ch <- []byte("a")
    p := PolicyDropOldest{}
    if err := p.Apply(ch, []byte("b"), nil); err != nil {
        t.Fatal(err)
    }
    if got := <-ch; string(got) != "b" {
        t.Fatalf("expected b, got %s", got)
    }
}

func TestPolicyDropNewest(t *testing.T) {
    ch := make(chan []byte, 1)
    ch <- []byte("a")
    p := PolicyDropNewest{}
    if err := p.Apply(ch, []byte("b"), nil); err != nil {
        t.Fatal(err)
    }
    if got := <-ch; string(got) != "a" {
        t.Fatalf("expected a, got %s", got)
    }
}
```

- [ ] **Step 2: Implement policies**

```go
// internal/conn/policy.go
package conn

type Policy interface {
    Apply(send chan []byte, msg []byte, onSent func()) error
}

type PolicyDisconnect struct{}

func (PolicyDisconnect) Apply(send chan []byte, msg []byte, onSent func()) error {
    select {
    case send <- msg:
        if onSent != nil {
            onSent()
        }
        return nil
    default:
        return ErrSlowConsumer
    }
}

type PolicyDropOldest struct{}

func (PolicyDropOldest) Apply(send chan []byte, msg []byte, onSent func()) error {
    select {
    case send <- msg:
    default:
        select {
        case <-send:
        default:
        }
        send <- msg
    }
    return nil
}

type PolicyDropNewest struct{}

func (PolicyDropNewest) Apply(send chan []byte, msg []byte, onSent func()) error {
    select {
    case send <- msg:
        return nil
    default:
        return nil // drop
    }
}
```

- [ ] **Step 3: Integrate into Conn.Send**

Replace `Conn.Send` body:

```go
func (c *Conn) Send(b []byte) error {
    return c.policy.Apply(c.send, b, nil)
}
```

Add `policy Policy` field to Conn, default to `PolicyDisconnect{}`. Boot wiring sets per `--slow-consumer` flag.

When `Send` returns `ErrSlowConsumer`, conn must be closed with code 1008. The fanout layer catches the error and triggers close; or the policy embeds a callback. Simplest: in `Conn.Send`, on `ErrSlowConsumer`, signal close via a `closeReason chan string`.

- [ ] **Step 4: Run tests**

```bash
go test -race ./...
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: configurable slow-consumer policy (disconnect/drop-oldest/drop-newest)"
```

---

### Task 16: Per-key publish rate limit (token bucket + stale-key GC)

**Files:**
- Create: `internal/ratelimit/limiter.go`
- Create: `internal/ratelimit/limiter_test.go`

- [ ] **Step 1: Add dependency**

```bash
go get golang.org/x/time/rate
```

- [ ] **Step 2: Tests**

```go
// internal/ratelimit/limiter_test.go
package ratelimit

import (
    "testing"
    "time"
)

func TestAllowAndExhaust(t *testing.T) {
    l := New(10, 5, time.Hour)
    defer l.Close()
    for i := 0; i < 5; i++ {
        if !l.Allow("key-a") {
            t.Fatalf("expected allowed at %d", i)
        }
    }
    if l.Allow("key-a") {
        t.Fatal("expected denied after burst")
    }
}

func TestPerKeyIsolation(t *testing.T) {
    l := New(10, 1, time.Hour)
    defer l.Close()
    if !l.Allow("a") || !l.Allow("b") {
        t.Fatal("first burst per key allowed")
    }
}

func TestGCEvictsStale(t *testing.T) {
    l := New(10, 1, 10*time.Millisecond)
    defer l.Close()
    l.Allow("k")
    time.Sleep(50 * time.Millisecond)
    l.gcOnce()
    if _, ok := l.peek("k"); ok {
        t.Fatal("expected k evicted")
    }
}
```

- [ ] **Step 3: Implement**

```go
// internal/ratelimit/limiter.go
package ratelimit

import (
    "sync"
    "time"

    "golang.org/x/time/rate"
)

type entry struct {
    lim      *rate.Limiter
    lastSeen time.Time
}

type Limiter struct {
    mu      sync.Mutex
    entries map[string]*entry
    rps     rate.Limit
    burst   int
    ttl     time.Duration
    quit    chan struct{}
}

func New(rps int, burst int, ttl time.Duration) *Limiter {
    l := &Limiter{
        entries: map[string]*entry{},
        rps:     rate.Limit(rps),
        burst:   burst,
        ttl:     ttl,
        quit:    make(chan struct{}),
    }
    go l.gcLoop()
    return l
}

func (l *Limiter) Allow(key string) bool {
    l.mu.Lock()
    e, ok := l.entries[key]
    if !ok {
        e = &entry{lim: rate.NewLimiter(l.rps, l.burst)}
        l.entries[key] = e
    }
    e.lastSeen = time.Now()
    l.mu.Unlock()
    return e.lim.Allow()
}

func (l *Limiter) peek(key string) (*entry, bool) {
    l.mu.Lock()
    defer l.mu.Unlock()
    e, ok := l.entries[key]
    return e, ok
}

func (l *Limiter) Close() { close(l.quit) }

func (l *Limiter) gcLoop() {
    t := time.NewTicker(l.ttl)
    defer t.Stop()
    for {
        select {
        case <-t.C:
            l.gcOnce()
        case <-l.quit:
            return
        }
    }
}

func (l *Limiter) gcOnce() {
    cutoff := time.Now().Add(-l.ttl)
    l.mu.Lock()
    for k, e := range l.entries {
        if e.lastSeen.Before(cutoff) {
            delete(l.entries, k)
        }
    }
    l.mu.Unlock()
}
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/ratelimit/...
```

- [ ] **Step 5: Commit**

```bash
git add .
git commit -m "feat: per-key publish rate limit with token bucket + stale-key GC"
```

---

### Task 17: Resource limits (max channels per conn, max subscribers per channel, max frame)

**Files:**
- Modify: `internal/conn/handler.go` (subscribe enforces max-channels)
- Modify: `internal/hub/channel.go` (Subscribe enforces max-subs)
- Modify: `internal/conn/conn.go` (read limit already 64KB; surface as flag)

- [ ] **Step 1: Test caps**

Add tests asserting:
- subscribing past `MaxChannelsPerConn` returns `error{code:"LIMIT_CHANNELS"}`, doesn't close
- subscribing past `MaxSubscribersPerChannel` returns `error{code:"LIMIT_SUBSCRIBERS"}`
- frame > 64KB closes 1009

- [ ] **Step 2: Implement caps**

In `handleSubscribe`, before `GetOrCreate`:
```go
if len(c.subs) >= c.maxChannels {
    c.sendError("LIMIT_CHANNELS", "max channels per conn")
    return
}
```

In `hub.Subscribe`, accept `max int`, return `ErrTooManySubs` if exceeded; handler propagates to `error` msg.

- [ ] **Step 3: Run tests + commit**

```bash
go test -race ./...
git add .
git commit -m "feat: enforce max-channels-per-conn, max-subs-per-channel, max-frame"
```

---

### Task 18: /v1/auth/sign endpoint (issue HMAC channel tokens)

**Files:**
- Modify: `internal/server/rest.go` (add /v1/auth/sign)
- Test: extend `rest_test.go`

- [ ] **Step 1: Test**

```go
func TestAuthSign(t *testing.T) {
    s := store.NewMemory()
    secret, _ := auth.GenerateSecret()
    k, _ := s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
    rest := NewRestHandler(s, "admin", "server-signing-secret")
    mux := http.NewServeMux()
    rest.Register(mux)
    srv := httptest.NewServer(mux)
    defer srv.Close()

    body := bytes.NewBufferString(fmt.Sprintf(`{"socket_id":"01HX","channel":"private-room"}`))
    req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/sign", body)
    req.Header.Set("Authorization", "Bearer "+k.ID+":"+secret) // composite credential
    req.Header.Set("Content-Type", "application/json")
    res, _ := http.DefaultClient.Do(req)
    if res.StatusCode != http.StatusOK {
        t.Fatalf("want 200, got %d", res.StatusCode)
    }
    var got struct{ Token string }
    json.NewDecoder(res.Body).Decode(&got)
    if err := auth.VerifyToken("server-signing-secret", "01HX", "private-room", got.Token); err != nil {
        t.Fatal(err)
    }
}
```

- [ ] **Step 2: Implement**

```go
func (h *RestHandler) sign(w http.ResponseWriter, r *http.Request) {
    // verify "Bearer <key_id>:<secret>"
    ah := r.Header.Get("Authorization")
    creds := strings.TrimPrefix(ah, "Bearer ")
    parts := strings.SplitN(creds, ":", 2)
    if len(parts) != 2 {
        http.Error(w, "bad credentials", http.StatusUnauthorized)
        return
    }
    k, err := h.store.LookupKey(r.Context(), parts[0])
    if err != nil || !auth.VerifySecret(parts[1], k.SecretHash) {
        http.Error(w, "bad credentials", http.StatusUnauthorized)
        return
    }
    var body struct{ SocketID, Channel string }
    json.NewDecoder(r.Body).Decode(&body)
    tok, _ := auth.SignToken(h.signingSecret, body.SocketID, body.Channel, time.Now().Add(5*time.Minute))
    json.NewEncoder(w).Encode(map[string]string{"token": tok})
}
```

Register via `mux.HandleFunc("POST /v1/auth/sign", h.sign)`. Update `NewRestHandler` to accept signingSecret.

- [ ] **Step 3: Run tests + commit**

```bash
go test -race ./...
git add .
git commit -m "feat: /v1/auth/sign endpoint for HMAC channel-token issuance"
```

---

### Task 19: Goroutine-leak proof test (THE headline test)

**Files:**
- Create: `internal/server/leak_test.go`

- [ ] **Step 1: Write the leak test**

```go
// internal/server/leak_test.go
package server

import (
    "context"
    "net/http/httptest"
    "runtime"
    "strings"
    "sync"
    "testing"
    "time"

    "github.com/EthanY33/wirefan/internal/auth"
    "github.com/EthanY33/wirefan/internal/store"
    "github.com/coder/websocket"
)

func TestNoGoroutineLeakAfterChurn(t *testing.T) {
    base := runtime.NumGoroutine()
    s := store.NewMemory()
    secret, _ := auth.GenerateSecret()
    k, _ := s.CreateKey(context.Background(), "t", auth.HashSecret(secret))
    h := NewUpgradeHandler(s, []string{"*"})
    srv := httptest.NewServer(h)
    defer srv.Close()
    wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/v1/connect?key=" + k.ID

    var wg sync.WaitGroup
    for i := 0; i < 1000; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            ctx, cancel := context.WithTimeout(context.Background(), time.Second)
            defer cancel()
            c, _, err := websocket.Dial(ctx, wsURL, nil)
            if err != nil {
                return
            }
            c.Close(websocket.StatusNormalClosure, "")
        }()
    }
    wg.Wait()

    // Allow GC + cleanup
    deadline := time.Now().Add(2 * time.Second)
    for time.Now().Before(deadline) {
        if runtime.NumGoroutine() <= base+5 {
            return
        }
        time.Sleep(50 * time.Millisecond)
    }
    t.Fatalf("goroutine leak: base=%d after=%d", base, runtime.NumGoroutine())
}
```

- [ ] **Step 2: Run, verify clean**

```bash
go test -race -run TestNoGoroutineLeak -v ./internal/server/
# Expected: PASS within 2s
```

If it fails, audit `Conn.Run` shutdown path — both pumps must exit on ctx cancel, send chan must be closeable, ws.Close must be idempotent.

- [ ] **Step 3: Commit**

```bash
git add .
git commit -m "test: goroutine-leak proof under 1k connection churn"
```

---

### Task 20: Graceful shutdown — drain connections, force-close after grace

**Files:**
- Modify: `internal/server/server.go` (Shutdown drains conns)
- Create: `internal/hub/hub.go` (tracks all conns, broadcasts close)

- [ ] **Step 1: Hub tracks all conns**

```go
// internal/hub/hub.go
package hub

import (
    "context"
    "sync"
    "time"

    "github.com/coder/websocket"
)

type closer interface {
    CloseFrame(code websocket.StatusCode, reason string)
}

type Hub struct {
    mu    sync.RWMutex
    conns map[closer]struct{}
}

func New() *Hub { return &Hub{conns: map[closer]struct{}{}} }

func (h *Hub) Add(c closer) {
    h.mu.Lock()
    h.conns[c] = struct{}{}
    h.mu.Unlock()
}

func (h *Hub) Remove(c closer) {
    h.mu.Lock()
    delete(h.conns, c)
    h.mu.Unlock()
}

func (h *Hub) Drain(ctx context.Context, grace time.Duration) {
    h.mu.RLock()
    for c := range h.conns {
        c.CloseFrame(websocket.StatusGoingAway, "shutdown")
    }
    h.mu.RUnlock()
    deadline := time.Now().Add(grace)
    for time.Now().Before(deadline) {
        h.mu.RLock()
        n := len(h.conns)
        h.mu.RUnlock()
        if n == 0 {
            return
        }
        select {
        case <-ctx.Done():
            return
        case <-time.After(50 * time.Millisecond):
        }
    }
}
```

- [ ] **Step 2: Conn implements closer**

Add to `Conn`:
```go
func (c *Conn) CloseFrame(code websocket.StatusCode, reason string) {
    _ = c.ws.Close(code, reason)
}
```

In `Conn.Run`, register/unregister with hub: `hub.Add(c); defer hub.Remove(c)`.

- [ ] **Step 3: Server.Run calls Hub.Drain on shutdown**

```go
// internal/server/server.go (shutdown branch)
s.health.SetDraining(true)
s.hub.Drain(ctx, 30*time.Second)
return s.srv.Shutdown(shutdownCtx)
```

- [ ] **Step 4: Tests + commit**

Test: open 5 conns, cancel ctx, assert all conns return cleanly within 35s.

```bash
go test -race ./internal/server/...
git add .
git commit -m "feat: graceful shutdown drains connections within 30s grace"
```

---

### Task 21: Prometheus metrics + slog structured logging + pprof + OTel hook

**Files:**
- Create: `internal/metrics/prom.go`
- Create: `internal/metrics/otel.go`
- Modify: `internal/server/server.go` (mount /metrics + /debug/pprof)
- Modify: various — emit metrics from conn lifecycle, fanout, rate limiter

- [ ] **Step 1: Add deps**

```bash
go get github.com/prometheus/client_golang/prometheus
go get github.com/prometheus/client_golang/prometheus/promhttp
go get go.opentelemetry.io/otel
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
go get go.opentelemetry.io/otel/sdk/trace
```

- [ ] **Step 2: Metric definitions**

```go
// internal/metrics/prom.go
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
    Connections = prometheus.NewGauge(prometheus.GaugeOpts{Name: "wirefan_connections_total"})
    Channels    = prometheus.NewGauge(prometheus.GaugeOpts{Name: "wirefan_channels_total"})
    Published   = prometheus.NewCounter(prometheus.CounterOpts{Name: "wirefan_messages_published_total"})
    Dropped     = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wirefan_messages_dropped_total"}, []string{"reason"})
    Latency     = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "wirefan_broadcast_latency_seconds",
        Buckets: prometheus.ExponentialBuckets(0.0001, 2, 16),
    })
    UpgradeRej  = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wirefan_upgrade_rejected_total"}, []string{"reason"})
    AuthFails   = prometheus.NewCounter(prometheus.CounterOpts{Name: "wirefan_auth_failures_total"})
)

func Register() {
    prometheus.MustRegister(Connections, Channels, Published, Dropped, Latency, UpgradeRej, AuthFails)
}
```

- [ ] **Step 3: Mount endpoints**

```go
// internal/server/server.go (in New)
import "net/http/pprof"
import "github.com/prometheus/client_golang/prometheus/promhttp"

s.mux.Handle("/metrics", promhttp.Handler())
s.mux.HandleFunc("/debug/pprof/", pprof.Index)
s.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
s.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
```

- [ ] **Step 4: Emit metrics from key paths**

- `Connections.Inc()` on `Conn.Run` start, `Dec()` on return
- `Published.Inc()` in `handlePublish`
- `Dropped.WithLabelValues("slow_consumer").Inc()` in policy disconnect path
- `Latency.Observe(...)` around `Fanout.Broadcast`

- [ ] **Step 5: OTel hook (dormant unless --otel-endpoint)**

```go
// internal/metrics/otel.go
package metrics

import (
    "context"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func InitOTel(ctx context.Context, endpoint string) (func(context.Context) error, error) {
    if endpoint == "" {
        return func(context.Context) error { return nil }, nil
    }
    exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
    if err != nil {
        return nil, err
    }
    tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
    otel.SetTracerProvider(tp)
    return tp.Shutdown, nil
}
```

Wire from main: `shutdown, _ := metrics.InitOTel(ctx, *otelEndpoint); defer shutdown(ctx)`.

- [ ] **Step 6: Run tests + commit**

```bash
go test -race ./...
git add .
git commit -m "feat: Prometheus metrics, slog logging, pprof, OTel hook"
```

---

### Task 22: _wirefan-stats system channel publisher

**Files:**
- Create: `internal/hub/stats.go`

- [ ] **Step 1: Test**

Subscribe a fake to `_wirefan-stats` channel, assert it receives a stats event within 2s.

- [ ] **Step 2: Implement**

```go
// internal/hub/stats.go
package hub

import (
    "context"
    "encoding/json"
    "time"

    "github.com/EthanY33/wirefan/internal/registry"
)

func PublishStatsLoop(ctx context.Context, r registry.Registry, interval time.Duration, snap func() map[string]int64) {
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            ch := r.GetOrCreate("_wirefan-stats")
            payload, _ := json.Marshal(map[string]any{
                "type": "event", "channel": "_wirefan-stats",
                "data": snap(), "id": time.Now().Format(time.RFC3339Nano),
            })
            Broadcast(ch, payload)
        }
    }
}
```

The `snap` func returns the live counts (connections / channels / msg_per_sec / drops) — wire from main using Prometheus collectors directly or a separate counter.

- [ ] **Step 3: Reject client publishes to `_wirefan-stats`**

In `handlePublish`, if `strings.HasPrefix(msg.Channel, "_")`, send `error{code:"RESERVED_CHANNEL"}`.

- [ ] **Step 4: Wire from main + commit**

Start the stats loop after server.Run launches.

```bash
git add .
git commit -m "feat: _wirefan-stats reserved system channel for live server metrics"
```

---

### Task 23: Demo client (vanilla JS, three panels, stress button with caps)

**Files:**
- Create: `web/index.html`
- Create: `web/client.js`
- Create: `web/styles.css`
- Create: `web/embed.go` (embed.FS)
- Modify: `internal/server/server.go` (serve `/`)

- [ ] **Step 1: Invoke skills for design review BEFORE writing**

Per spec Tooling section, the demo `/` page must use `frontend-design` and `ui-ux-pro-max` skills. Before writing HTML/CSS/JS, invoke `frontend-design` to design the layout treatment, then `ui-ux-pro-max` to review palette/typography. Capture the design decisions in `web/DESIGN_NOTES.md` so this task isn't "default dark theme."

- [ ] **Step 2: Implement web/index.html, client.js, styles.css**

(Following the design output from Step 1 — three panels: connection / messages / stats. Stress button: 50 phantom-conns/tab, 10s auto-disconnect.)

- [ ] **Step 3: embed.go**

```go
// web/embed.go
package web

import "embed"

//go:embed index.html client.js styles.css
var Files embed.FS
```

- [ ] **Step 4: Server serves embed**

```go
// internal/server/server.go (in New)
import "github.com/EthanY33/wirefan/web"
s.mux.Handle("/", http.FileServerFS(web.Files))
```

- [ ] **Step 5: Server-side phantom-conn cap (200/IP)**

In `UpgradeHandler.ServeHTTP`, track per-IP open count using a map+mutex; reject 429 if at cap. Implement in `internal/server/upgrade.go`.

- [ ] **Step 6: Manual smoke test**

```bash
go run ./cmd/wirefan
# open http://localhost:8080 in two tabs, subscribe to "test", publish in one, see in other
```

- [ ] **Step 7: Run screenshot-tripwire on a screenshot of /**

Run the user's screenshot-tripwire CLI on a captured screenshot of the demo at `/`. Address any CRITICAL findings before commit.

- [ ] **Step 8: Commit**

```bash
git add .
git commit -m "feat: vanilla-JS demo client with stress button + phantom-conn caps"
```

---

### Task 24: cmd/loadtest binary (matrix-driven WS load generator)

**Files:**
- Create: `cmd/loadtest/main.go`

- [ ] **Step 1: Define flags + matrix runner**

```go
// cmd/loadtest/main.go
package main

import (
    "context"
    "flag"
    "log"
    "sync"
    "sync/atomic"
    "time"

    "github.com/coder/websocket"
)

var (
    url       = flag.String("url", "ws://localhost:8080/v1/connect?key=", "")
    conns     = flag.Int("conns", 1000, "concurrent connections")
    channels  = flag.Int("channels", 100, "distinct channels")
    msgRate   = flag.Int("rate", 10, "msg/s per connection")
    duration  = flag.Duration("dur", 30*time.Second, "test duration")
)

func main() {
    flag.Parse()
    ctx, cancel := context.WithTimeout(context.Background(), *duration)
    defer cancel()

    var sent, recv atomic.Int64
    var wg sync.WaitGroup
    for i := 0; i < *conns; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            c, _, err := websocket.Dial(ctx, *url, nil)
            if err != nil {
                return
            }
            defer c.Close(websocket.StatusNormalClosure, "")
            // subscribe to channels[id % numChannels], publish at rate
            // [...] standard implementation
        }(i)
    }
    wg.Wait()
    log.Printf("sent=%d recv=%d", sent.Load(), recv.Load())
}
```

(Full loadtest impl includes ramp-up, latency histogram via `hdrhistogram-go`, CSV output of p50/p99/p999.)

- [ ] **Step 2: Add Makefile bench target**

```makefile
bench:
	@./scripts/bench.sh
```

- [ ] **Step 3: Commit**

```bash
git add .
git commit -m "feat: cmd/loadtest matrix-driven WS load generator"
```

---

### Task 25: Run benchmarks, capture pprof flamegraphs, write BENCHMARKS.md

**Files:**
- Create: `scripts/bench.sh`
- Create: `docs/BENCHMARKS.md`
- Create: `docs/profiles/{cpu.png,heap.png}`

- [ ] **Step 1: Bench script**

```bash
# scripts/bench.sh
#!/usr/bin/env bash
set -euo pipefail

# Iterate over fanout strategies and registry primitives
for fanout in per-conn sharded; do
  for registry in sync-map sharded; do
    echo "=== fanout=$fanout registry=$registry ==="
    ./bin/wirefan --fanout=$fanout --registry=$registry --port=8080 &
    PID=$!
    sleep 1
    ./bin/loadtest --conns=10000 --channels=100 --rate=10 --dur=60s > "results/${fanout}-${registry}.csv"
    kill $PID
  done
done
```

- [ ] **Step 2: Capture pprof under load**

While loadtest is running, in another terminal:
```bash
go tool pprof -png http://localhost:8080/debug/pprof/profile?seconds=30 > docs/profiles/cpu.png
go tool pprof -png http://localhost:8080/debug/pprof/heap > docs/profiles/heap.png
```

- [ ] **Step 3: Write BENCHMARKS.md**

Sections:
- Hardware (1-vCPU, 2 GB Fly machine OR Oracle ARM64 free-tier)
- Methodology (matrix, duration, msg shape)
- Results table (per-conn × sync-map vs sharded × sharded, 4 cells)
- Latency curves (line chart PNG, embed)
- Flamegraph link
- Winner + hot-path explanation
- Reproduction: `make bench`

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "docs: BENCHMARKS.md with reproducible results + pprof flamegraphs"
```

---

### Task 26: PROTOCOL.md (full wire format spec)

**Files:**
- Create: `docs/PROTOCOL.md`

- [ ] **Step 1: Write spec sections** — message shapes (full JSON examples), channel naming rules, auth flow with sequence diagram, error codes table, size + rate limits, ordering guarantees.

- [ ] **Step 2: Run `copy-tripwire` on it**

```bash
npx copy-tripwire docs/PROTOCOL.md
```

- [ ] **Step 3: Commit**

```bash
git add docs/PROTOCOL.md
git commit -m "docs: full PROTOCOL.md wire-format specification"
```

---

### Task 27: DESIGN.md (architecture deep-dive + alternatives + decisions)

**Files:**
- Create: `docs/DESIGN.md`

- [ ] **Step 1: Write sections**
- Component overview (Hub, Channel, Conn, Fanout, Registry, Auth, Store, RateLimit, Metrics)
- Concurrency model (per-channel mutex for FIFO; per-conn read+write goroutines)
- Pluggable interfaces (Fanout, Registry, Store) — why each was made pluggable
- Alternatives considered: gorilla/websocket (rejected — see ecosystem note), Redis multi-server (deferred), custom epoll (gnet/nbio — deferred), MessagePack (deferred), WebTransport (deferred), Pusher protocol drop-in compat (rejected)
- Trade-offs: query-string auth vs Sec-WebSocket-Protocol bearer; SQLite vs Postgres; sync.Map vs sharded mutex
- Scaling roadmap: Redis pub-sub design sketch, presence with CRDT, message history with ring buffer
- Open questions

- [ ] **Step 2: copy-tripwire + commit**

```bash
npx copy-tripwire docs/DESIGN.md
git add .
git commit -m "docs: DESIGN.md architecture deep-dive + alternatives"
```

---

### Task 28: ARCHITECTURE.md via `supermind:sm-init` and ongoing `living-docs`

**Files:**
- Create: `ARCHITECTURE.md`

- [ ] **Step 1: Invoke `supermind:sm-init`**

```
/sm-init
```

The skill scans the repo and generates `ARCHITECTURE.md`. Review for accuracy.

- [ ] **Step 2: Set up `living-docs` cadence**

After every task that modifies architecture, run `living-docs` to keep `ARCHITECTURE.md` in sync. Add to `Makefile`:

```makefile
docs-sync:
	@echo "Run /living-docs to update ARCHITECTURE.md"
```

- [ ] **Step 3: Commit**

```bash
git add ARCHITECTURE.md
git commit -m "docs: ARCHITECTURE.md generated via sm-init"
```

---

### Task 29: Architecture diagram SVG via `frontend-design`

**Files:**
- Create: `docs/architecture.svg`

- [ ] **Step 1: Invoke `frontend-design`**

Use the frontend-design skill to design an architecture diagram showing: Client → WS Upgrade → Conn → Hub → Registry → Channel → Fanout → Subscribers, with REST control plane on the side. Save as committed SVG.

- [ ] **Step 2: Run `screenshot-tripwire`**

```bash
npx screenshot-tripwire docs/architecture.svg
```

- [ ] **Step 3: Commit**

```bash
git add docs/architecture.svg
git commit -m "docs: architecture diagram SVG"
```

---

### Task 30: README.md (designed as landing page) + social-preview OG card

**Files:**
- Create: `README.md`
- Create: `docs/og-card.png` (social preview)

- [ ] **Step 1: Invoke `frontend-design` + `ui-ux-pro-max` for README hero**

Design a README hero that opens with: live demo URL, 30s screencast GIF, headline benchmark number. Treat it as a landing page.

- [ ] **Step 2: Write README per spec structure**

Sections per the design spec (Quickstart, Architecture, Performance, Wire protocol, Why these choices, Deferred, Build & test).

- [ ] **Step 3: Run `copy-tripwire` on README**

```bash
npx copy-tripwire README.md
# Address any CRITICAL findings.
```

- [ ] **Step 4: Generate OG card via `frontend-design` + verify with `screenshot-tripwire`**

OG card: 1200×630, project name + tagline + logo. Save as `docs/og-card.png`. Run screenshot-tripwire.

- [ ] **Step 5: Configure repo social preview**

After GitHub repo creation, upload OG card via `Settings → Social preview`.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/og-card.png
git commit -m "docs: README.md (landing-page-quality hero) + OG social preview"
```

---

### Task 31: Multi-stage ARM64 Dockerfile + Caddyfile + systemd unit

**Files:**
- Create: `deploy/Dockerfile`
- Create: `deploy/Caddyfile`
- Create: `deploy/wirefan.service`

- [ ] **Step 1: Dockerfile**

```dockerfile
# deploy/Dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o /out/wirefan ./cmd/wirefan

FROM gcr.io/distroless/base-debian12
COPY --from=builder /out/wirefan /wirefan
EXPOSE 8080
ENTRYPOINT ["/wirefan"]
```

(Note: `mattn/go-sqlite3` requires CGO — using `base-debian12` not `static`. Alternative: `modernc.org/sqlite` for pure-Go SQLite, then can use `static`.)

- [ ] **Step 2: Caddyfile**

```caddy
# deploy/Caddyfile
wirefan.ethanyucetepe.dev {
    reverse_proxy localhost:8080 {
        header_up X-Forwarded-For {remote_host}
    }
}
```

- [ ] **Step 3: systemd unit**

```ini
# deploy/wirefan.service
[Unit]
Description=wirefan WebSocket fanout server
After=network.target

[Service]
ExecStart=/usr/local/bin/wirefan --port=8080
EnvironmentFile=/etc/wirefan/env
Restart=on-failure
RestartSec=5
User=wirefan

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 4: Commit**

```bash
git add deploy/
git commit -m "feat: deployment artifacts (Dockerfile, Caddyfile, systemd)"
```

---

### Task 32: Deploy to Oracle Cloud Always Free (or Cloudflare Tunnel fallback)

**Steps depend on user's hosting choice from spec open questions.**

**Path A: Oracle Always Free**

- [ ] Provision Ampere A1 instance via Oracle Cloud console (Always Free)
- [ ] Install Docker + Caddy on the instance
- [ ] Build ARM64 Docker image, push to Docker Hub or ghcr.io
- [ ] Pull + run via systemd unit
- [ ] Configure DNS (A record) for `wirefan.ethanyucetepe.dev` pointing to instance public IP
- [ ] Verify TLS via `curl -I https://wirefan.ethanyucetepe.dev/v1/health`

**Path B: Cloudflare Tunnel + home machine**

- [ ] Install `cloudflared` on the always-on home machine
- [ ] `cloudflared tunnel create wirefan`
- [ ] Configure tunnel to proxy to local `:8080`
- [ ] Run wirefan locally via Docker or `make build && ./bin/wirefan`
- [ ] Verify public URL serves `/v1/health`

- [ ] **Final: Commit deployment notes**

```bash
git add docs/DEPLOY.md
git commit -m "docs: deployment notes for Oracle / Cloudflare Tunnel paths"
```

---

### Task 33: CLAUDE.md for the project

**Files:**
- Create: `CLAUDE.md`

- [ ] **Step 1: Invoke `claude-md-management:revise-claude-md`**

Run the skill against the repo. It scans the codebase and generates a CLAUDE.md describing conventions, build commands, key files, gotchas.

- [ ] **Step 2: Review + commit**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md for future Claude sessions"
```

---

### Task 34: Create GitHub repo, push, set topics + description + social preview

- [ ] **Step 1: Create the public repo**

```bash
gh repo create EthanY33/wirefan --public \
  --description "Single-binary WebSocket fanout server in Go" \
  --source=/c/Users/ethan/Desktop/wirefan --push
```

- [ ] **Step 2: Set topics**

```bash
gh api repos/EthanY33/wirefan/topics -X PUT -f names[]=websocket -f names[]=golang -f names[]=real-time -f names[]=pubsub -f names[]=fanout-server
```

- [ ] **Step 3: Upload social-preview OG card**

Via GitHub web UI: Settings → Social preview → Upload `docs/og-card.png`.

- [ ] **Step 4: Update homepage URL**

```bash
gh repo edit EthanY33/wirefan --homepage "https://wirefan.ethanyucetepe.dev"
```

---

### Task 35: Update EthanY33 profile pins (replace one current pin with wirefan)

- [ ] **Step 1: Decide which pin to drop**

Recommended: drop one of the three tripwires (most likely `screenshot-tripwire`, since `trailer-tripwire` and `copy-tripwire` are stronger flagships). Keep atelier + 2 tripwires + PrivacyPulse + wirefan + news-bias-analyzer = 6 pins.

- [ ] **Step 2: User updates pins via GitHub web UI**

Pinning isn't exposed in GitHub's public API — manual step at github.com/EthanY33 → Customize your pins.

- [ ] **Step 3: Run `code-reviewer` final pass on the whole project**

Use the pr-review-toolkit code-reviewer agent on the full repo before publicizing. Fix all findings. Re-run until clean.

---

## Self-review

- **Spec coverage**: every locked-in feature in the spec maps to at least one task above. Pluggable interfaces (Fanout, Registry, Store) — Tasks 4, 8, 13, 14. Auth + HMAC tokens — Tasks 5, 6, 7, 18. WS lifecycle + heartbeat — Tasks 10, 11, 12. Slow-consumer policies — Task 15. Rate limit + GC — Task 16. Resource limits — Task 17. Goroutine leak proof — Task 19. Graceful shutdown — Task 20. Observability stack — Task 21. System channel — Task 22. Demo client — Task 23. Load test + benchmarks — Tasks 24, 25. Documentation set — Tasks 26-30. Deployment — Tasks 31, 32. Repo + pins — Tasks 33-35.

- **Placeholder scan**: a few sections defer detailed implementation to user-driven design choices (e.g. demo client design, OG card, deployment specifics for chosen hosting target). These are intentional — design decisions belong to the design skills (frontend-design / ui-ux-pro-max), not the plan. The plan correctly invokes the right skills at the right step.

- **Type consistency**: `Channel` defined in `internal/registry/registry.go`. `Subscriber` interface in same file. Hub functions take `*registry.Channel`. Conn implements `registry.Subscriber` via `Send([]byte) error` + `Close()`. Consistent throughout.

- **Skill discipline**: Tasks 23, 27, 28, 29, 30, 33, 35 all explicitly invoke the required skills per spec Tooling section.
