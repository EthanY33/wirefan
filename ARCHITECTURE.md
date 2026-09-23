# wirefan architecture

> Developer onboarding for **navigating the code**. For *why* decisions
> were made, see [`docs/DESIGN.md`](./docs/DESIGN.md). For the on-the-wire
> contract, see [`docs/PROTOCOL.md`](./docs/PROTOCOL.md). For what 1.x
> promises not to break, see [`docs/COMPATIBILITY.md`](./docs/COMPATIBILITY.md).
> For perf numbers, see [`docs/BENCHMARKS.md`](./docs/BENCHMARKS.md).
>
> This file answers: "I just cloned the repo; where do I look?"

---

## Quickstart for contributors

```bash
git clone https://github.com/EthanY33/wirefan && cd wirefan
go build ./...                            # sanity check; a working binary needs cgo (sqlite driver)
make build                                # -> bin/wirefan, version stamped from git describe
make test                                 # full suite (includes a 65 s keepalive test)
go test -short ./...                      # same, minus that test
make test-race                            # race-detector pass
./bin/wirefan --dev --allowed-origins='*' # public :8080, admin 127.0.0.1:6060
```

`--allowed-origins` is required; `'*'` is only accepted together with
`--dev`. The admin Bearer token is never printed: unless
`WIREFAN_ADMIN_TOKEN` is set, first boot writes it to `var/admin.token`
(or `$WIREFAN_STATE_DIR/admin.token`) and later boots reuse it; see
`cmd/wirefan/main.go: resolveAdminToken`. Use it to mint an API key
against the admin listener:

```bash
ADMIN=$(cat var/admin.token)
curl -s -XPOST http://127.0.0.1:6060/v1/keys \
  -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"dev"}'
# -> {"id":"01...","name":"dev","secret":"..."}   (id is a ULID, secret is 64 hex chars; the secret is shown once)
```

Open a WebSocket with `websocat` and play with the protocol:

```bash
KEY=01...                       # the "id" field from the mint response
websocat "ws://localhost:8080/v1/connect?key=$KEY"
> {"type":"subscribe","channel":"chat"}
> {"type":"publish","channel":"chat","data":{"hello":"world"}}
```

The bundled demo client lives at `http://localhost:8080/?key=<id>`
(served from `web/embed.go`).

---

## Repo map

```
wirefan/
├── cmd/
│   ├── wirefan/        main entry: flags (parseFlags), wiring (run), signal handling
│   └── loadtest/       standalone load generator driven by scripts/bench.sh
├── internal/
│   ├── auth/           API-key secret gen/hash, HMAC subscribe tokens, jti replay cache
│   ├── conn/           WS Conn lifecycle: pumps, keepalive, message router, backpressure policies
│   ├── fanout/         Broadcast strategies (inline per-conn, sharded worker pool)
│   ├── hub/            Live-conn set (Drain, CloseKey), channel Subscribe/Broadcast, stats publisher
│   ├── metrics/        Prometheus collectors
│   ├── ratelimit/      Per-key token-bucket limiter with background GC
│   ├── registry/       name → *Channel map (sync.Map and sharded impls), empty-channel sweep
│   ├── server/         HTTP servers and muxes, /v1/health, REST handlers, WS upgrade handler
│   └── store/          API-key persistence (memory + SQLite with versioned migrations)
├── clients/js/         @wirefan/client TypeScript SDK (own 0.x version, own tests)
├── web/                Embedded demo client (go:embed); wirefan-client.js is a vendored build of clients/js
├── deploy/             provision.sh, deploy.sh, systemd unit, Caddyfile, Dockerfile, .env.example
├── docs/               DESIGN, PROTOCOL, COMPATIBILITY, DEPLOY, BENCHMARKS; img/ (images, including
│                       the README diagrams), profiles/ (pprof text), superpowers/ (original spec + plan)
├── results/            Raw benchmark output and pprof captures behind docs/BENCHMARKS.md
├── scripts/bench.sh    Benchmark driver (used by `make bench`)
├── .github/workflows/  ci.yml (Go build/test/lint, govulncheck, JS client), release.yml (tag -> binaries)
└── Makefile            build / test / test-race / lint / clean / loadtest / bench-image / bench / release-local / docs-sync
```

There is no tracing. The OpenTelemetry hook was removed for 1.0 (it was
only ever called with an empty endpoint); that is a deliberate divergence
from locked-in feature 7 of the spec in `docs/superpowers/specs/`, which
called for an `--otel-endpoint` flag. Observability is Prometheus
`/metrics`, `log/slog` and pprof.

Top-level files of note:

- `go.mod`: module path is `github.com/EthanY33/wirefan`. Its
  `toolchain` line makes the go command build with at least that Go
  release, downloading it when the installed Go is older.
- `Makefile`: every contributor command is in here; if you find yourself
  typing a long `go ...` invocation more than once, add a target. GNU
  make is not on PATH in Git Bash on Windows, so the comments give the
  direct commands.
- `CHANGELOG.md`: `release.yml` builds the release notes from the
  `## [x.y.z]` section matching the tag and fails without one.
- `CONTRIBUTING.md`, `SECURITY.md`, `LICENSE` (MIT).

---

## Request lifecycle

A client connects, subscribes, then publishes. Here is the path through
the code, with file:symbol landmarks (symbols survive refactors; line
numbers do not).

**Boot.**

1. `cmd/wirefan/main.go: main`: sets up the SIGINT/SIGTERM-cancelable
   context and calls `parseFlags` (`--version` prints and exits).
2. `cmd/wirefan/main.go: run`: opens the store, resolves the admin token,
   generates the per-boot token-signing secret, picks the registry and
   fanout, builds the per-key rate limiter, the `hub.Hub` and the server
   (with `conn.PolicyDisconnect{}` as the backpressure policy). It starts
   `hub.PublishStatsLoop` (a `_wirefan-stats` event every 5 s, sent with
   `hub.Broadcast` directly, not through the fanout) and
   `registry.SweepLoop` (removes channels with no subscribers once a
   minute), then calls `Server.Run`.
3. `internal/server/server.go: New`: calls `metrics.Register` and
   registers routes. Public mux: `/v1/health`, `POST /v1/auth/sign`, WS
   at `/v1/connect`, and the embedded demo at `/`. Admin mux (separate
   listener, `--admin-addr`, default `127.0.0.1:6060`): `/v1/keys`,
   `/metrics`, `/debug/pprof/*`. Only the `/v1/keys` routes check the
   admin token; the rest rely on the listener being loopback or
   internal. The public `http.Server` gets
   `ConnContext: conn.WithNetConn`, so `conn.Run` can reach the TCP
   connection under each WebSocket.
4. `internal/server/server.go: Server.Run`: starts both `http.Server`s
   and a once-a-minute sweep of the token replay cache, then blocks until
   ctx is canceled or a listener fails.

**Connect.**

5. `internal/server/upgrade.go: UpgradeHandler.ServeHTTP`: looks up the
   `?key=` API key (401 if missing, unknown or revoked), applies the
   per-IP connection cap (`defaultIPCap`, overridable via
   `WIREFAN_IP_CAP`; IPv6 clients count per /64, see `ipCapKey`; 429),
   upgrades via `coder/websocket` (which enforces `--allowed-origins`),
   mints a ULID `socket_id`, and hands off to `conn.Run`.
6. `internal/conn/conn.go: Run`: builds the `Conn` with its per-conn rate
   limiters and registers it with the Hub (`Hub.Add` refuses a conn whose
   key `Hub.CloseKey` already closed, and Run sends it that close). It
   queues the `connected` hello frame, then spawns `writePump` and
   `readPump` (`internal/conn/pumps.go`). When a pump returns, or a
   slow-consumer close fires (step 11), Run stops both pumps, closes the
   socket and unsubscribes the conn from every channel.

**Subscribe / publish.**

7. `internal/conn/pumps.go`: `readPump` reads each message on the run
   context (64 KiB limit, no per-read deadline) and calls `c.handle`.
   `writePump` writes queued frames and pings every `pingInterval`
   (30 s); a pong not back within `pongWait` (10 s) ends the conn.
8. `internal/conn/handler.go: handle`: decodes the frame, checks the
   channel of a subscribe, unsubscribe or publish with
   `ValidateChannelName`, and dispatches on `type`:
   - `subscribe` → `handleSubscribe`. Rejects reserved `_*` channels
     except the read-only `_wirefan-stats` carve-out, and charges the
     per-conn control bucket, then the per-key bucket (`allowControl`).
     A channel the conn already holds is acked without a token.
     `private-*` and `presence-*` channels (`ChannelRequiresAuth`) need a
     socket-bound HMAC token, verified by `auth.VerifyTokenAgainst`
     (jti replay cache). Caps channels per conn at
     `defaultMaxChannelsPerConn` (`internal/conn/conn.go`). Calls
     `registry.GetOrCreate` then `hub.Subscribe`, retrying if the sweep
     deleted the channel in between.
   - `publish` → `handlePublish`. Rejects reserved `_*` channels,
     requires prior subscription, charges the per-conn publish bucket,
     then the per-key bucket, encodes the event with `marshalFrame`
     (ULID `id`, no HTML escaping), then calls `Fanout.Broadcast`.
   - `unsubscribe` → `handleUnsubscribe`. Charges the same buckets as
     subscribe, drops the subscription and acks.

   Errors answering these three frames carry `op` and `channel`
   (`sendOpError`; `channel` is left out when it is over 128 bytes).
   `BAD_JSON` and `BAD_TYPE` carry neither.

**Fanout.**

9. `internal/fanout/perconn.go` (default): inline `hub.Broadcast` on
   the publisher's own read goroutine. Zero extra hops.
10. `internal/fanout/sharded.go`: fixed pool of `GOMAXPROCS` workers; a
    broadcast is queued to the worker picked by a hash of the channel
    name, so one channel always uses the same worker.
11. `internal/hub/channel.go: Broadcast` snapshots the subscriber set
    under `SubsMu.RLock`, then calls `Subscriber.Send([]byte)` on each.
    For a `Conn`, `Send` runs the backpressure `Policy`
    (`internal/conn/policy.go: Policy`); on `ErrSlowConsumer` it counts
    a drop and signals `Run`, which closes the conn with WebSocket code
    1008 before stopping the pumps.

**Key revoke.**

12. `internal/server/rest.go: revoke` (`DELETE /v1/keys/{id}`): marks
    the key revoked in the store, then `Hub.CloseKey` closes every live
    conn opened with it (1008, `key revoked`) in background goroutines
    and bars the key from `Hub.Add` for the life of the process.

**Shutdown.**

13. SIGTERM cancels the root ctx. `Server.Run` flips `/v1/health` to
    503 (from then on `/v1/connect` also answers 503) and calls
    `Hub.Drain` (`internal/hub/hub.go: Drain`) with a 30 s limit: it bars
    `Hub.Add` so a reconnecting client cannot slip in, sends 1001
    `shutdown` to every tracked conn concurrently,
    waits for them to deregister, and force-closes (no close frame) any
    still open when the limit passes. Run then shuts down the admin
    listener, then the public one, closes the fanout (the sharded pool
    finishes its queued broadcasts), and returns. `run`'s deferred calls
    then close the rate limiter and the store.

### Where each metric is recorded

All collectors are declared in `internal/metrics/prom.go`. The names
are frozen for 1.x ([`docs/COMPATIBILITY.md`](./docs/COMPATIBILITY.md));
only counters end in `_total`.

| Metric | Type | Recorded in |
| ------ | ---- | ----------- |
| `wirefan_connections` | gauge | `internal/conn/conn.go: Run` |
| `wirefan_channels` | gauge, read at scrape time | `registry.Len`, installed by `metrics.SetChannelSource` in `cmd/wirefan/main.go: run` |
| `wirefan_messages_published_total` | counter | `internal/conn/handler.go: handlePublish` |
| `wirefan_broadcast_latency_seconds` | histogram | `handlePublish`, around `Fanout.Broadcast` |
| `wirefan_messages_dropped_total{reason}` | counter | `internal/conn/handler.go: Conn.Send` (`slow_consumer`) |
| `wirefan_upgrade_rejected_total{reason}` | counter | `internal/server/upgrade.go: ServeHTTP` (`bad_key`, `phantom_cap`, `draining`) |
| `wirefan_auth_failures_total` | counter | `internal/conn/handler.go: handleSubscribe` |

`metrics.SnapshotBasic` reads the same collectors for the
`_wirefan-stats` payload.

### Cross-package seams

- `conn.ValidateChannelName` and `conn.ChannelRequiresAuth` are the
  channel rules shared by the WS handler and `POST /v1/auth/sign`
  (`internal/server/rest.go: sign`, which also requires `socket_id` to
  be a ULID).
- `conn.WithNetConn` is the `http.Server.ConnContext` hook `server.New`
  installs.
- `hub.Hub`: `Add` returns `(websocket.CloseError, bool)`, false plus the
  close to send when `Drain` has started (1001) or `CloseKey` already ran
  for the conn's key; `Remove`;
  `Len`; `CloseKey(keyID, code, reason)` returns how many conns it
  closed; `Drain(ctx, grace)`. Tracked conns implement the unexported
  `trackedConn` interface (`APIKeyID`, `CloseFrame`, `CloseNow`), which
  `*conn.Conn` provides.
- `server.NewRestHandler(store, adminToken, signingSecret, *hub.Hub)`
  takes the Hub so a revoke can close live conns.
- `registry.Registry` includes `CompareAndDelete`, which `Sweep` uses to
  remove an empty channel while holding its `SubsMu`.

---

## Where to look when

| Goal                                | File                                                  |
| ----------------------------------- | ----------------------------------------------------- |
| Add a new client→server message     | `internal/conn/handler.go` (extend `incoming` + `handle`) |
| Add a new HTTP route                | `internal/server/rest.go` (`RegisterPublic` / `RegisterAdmin`), or `server.go: New` for top-level |
| Tune backpressure / write a Policy  | `internal/conn/policy.go`; the active policy is set in `cmd/wirefan/main.go: run` (no flag) |
| Tune the per-key rate limit         | `ratelimit.New(100, 200, time.Hour)` in `cmd/wirefan/main.go: run` |
| Tune the per-conn rate limits       | `internal/conn/conn.go` constants (`defaultConnPublishRate`, `defaultConnControlRate`, ...) |
| Change keepalive timing             | `internal/conn/conn.go`: `pingInterval`, `pongWait` (package vars; there is no read deadline) |
| Add a Prometheus metric             | `internal/metrics/prom.go` (declare the collector and add it to `realRegister`) |
| Modify the wire format              | `internal/conn/handler.go` **and update `docs/PROTOCOL.md`**; check `docs/COMPATIBILITY.md` first |
| Encode a new outbound frame         | `internal/conn/handler.go: marshalFrame` (no HTML escaping)  |
| Tweak resource limits (channels/subs)| `internal/conn/conn.go` constants; channel-name cap `maxChannelNameLen` in `handler.go`; 64 KiB message cap in `pumps.go: readPump` |
| Change the IP phantom-conn cap      | `WIREFAN_IP_CAP` env (`internal/server/upgrade.go: defaultIPCap`, `ipCapKey`) |
| Swap fanout strategy                | `--fanout=per-conn\|sharded` (`cmd/wirefan/main.go: newFanout`) |
| Swap registry impl                  | `--registry=sync-map\|sharded` (`cmd/wirefan/main.go: newRegistry`) |
| Swap store backend                  | `--store=sqlite\|memory`, `--db-path` (`cmd/wirefan/main.go: openStore`) |
| Change the key-database schema      | `internal/store/migrate.go: migrations` (append-only, contiguous versions) |
| Add an admin REST endpoint          | `internal/server/rest.go: RestHandler.RegisterAdmin` (wrap it in `requireAdmin`) |
| Modify shutdown behavior            | `internal/server/server.go: Server.Run` + `internal/hub/hub.go: Drain` |
| Modify what a key revoke does       | `internal/server/rest.go: revoke` + `internal/hub/hub.go: CloseKey` |
| Sign HMAC channel tokens            | `internal/auth/token.go` (MAC layout in `macPayload`)  |
| Edit the demo client UI             | `web/index.html`, `web/client.js`, `web/styles.css`   |
| Change the JS client                | `clients/js/src/index.ts`, then `npm run vendor:web` to regenerate `web/wirefan-client.js` (CI fails if it is stale) |

---

## Test layout

Tests live next to the code they cover (`*_test.go`). Highlights:

- `internal/server/leak_test.go`: goroutine-leak proof.
  `TestNoGoroutineLeakAfterChurn` opens and closes 1,000 WS conns under
  each fanout and asserts `runtime.NumGoroutine` comes back within a
  small tolerance (30) of baseline; `TestNoGoroutineLeakAfterHubCloses`
  does the same with 100 conns each for `Hub.CloseKey`, `Hub.Drain` and
  conns refused at `Hub.Add`. Run one alone:
  ```bash
  go test -run TestNoGoroutineLeakAfterChurn ./internal/server -v
  ```
- `internal/server/shutdown_test.go`: `Hub.Drain` closes all conns,
  returns within its ctx when peers never read, and frees the socket of a
  peer stalled mid-frame.
- `internal/server/health_test.go`: `/v1/health` 200, and 503 while
  draining.
- `internal/server/rest_test.go`: key admin, revoke closing live conns,
  `/v1/auth/sign` input checks.
- `internal/server/upgrade_test.go`: key check, trusted proxies and
  `X-Forwarded-For`, per-IP cap (IPv6 by /64).
- `internal/conn/handler_test.go`: protocol message dispatch, tokens,
  limits, error-frame `op`/`channel`, payloads not inflated by escaping.
- `internal/conn/pumps_test.go`: keepalive.
  `TestKeepaliveIdleReaderOutlivesOldCutoff` runs for 65 s at the
  production timers; `-short` skips it.
- `internal/conn/conn_test.go`: `Run` teardown (oversize message, key
  closed before `Hub.Add`, close handshake stalled mid-frame).
- `internal/conn/policy_test.go`: backpressure policies in isolation.
- `internal/fanout/*_test.go`: each fanout impl has its own tests (the
  leak test in `internal/server` runs under both).
- `internal/hub/*_test.go`: `Drain`, `CloseKey`, `Add` refusal, channel
  subscribe/broadcast, stats loop.
- `internal/registry/*_test.go`: both registry impls share one suite
  (`runRegistryTests`); `sweep_test.go` covers the sweep.
- `internal/store/*_test.go`: memory and SQLite share one suite
  (`runStoreTests`); `migrate_test.go` covers the migration runner and
  refusing a newer or foreign database.
- `internal/metrics/prom_test.go`: exposition names and help text, stats
  snapshot.
- `internal/ratelimit/limiter_test.go`: bucket math + GC eviction.
- `internal/auth/{keys,token}_test.go`: secret hashing, HMAC tokens,
  field-shift forgery, non-canonical jti.
- `cmd/wirefan/main_test.go`: flag parsing, `--version`, boot and clean
  exit, SQLite keys surviving a restart.
- `clients/js/test/`: unit tests plus an end-to-end suite against a real
  server binary (`WIREFAN_BIN`, or `wirefan`/`wirefan.exe` at the repo
  root; skipped with a notice otherwise).

Run subsets:

```bash
make test                         # everything
make test-race                    # everything, -race
go test -short ./...              # everything except the 65 s keepalive test
go test ./internal/conn/...       # one package tree
go test -run TestSubscribe ./...  # by name
cd clients/js && npm ci && npm test
```

---

## Build artifacts

`bin/` holds local binaries, `dist/` holds release builds:

- `bin/wirefan`: the server. Built by `make build`.
- `bin/loadtest`: the load generator. Built by `make loadtest`.
- `dist/wirefan_<version>_linux_{amd64,arm64}` and `dist/SHA256SUMS`:
  built by `make release-local`.

Make targets:

| Target          | What it does                                             |
| --------------- | -------------------------------------------------------- |
| `make build`    | `go build -ldflags "-X main.version=$(VERSION)" -o bin/wirefan ./cmd/wirefan`; `VERSION` defaults to `git describe --tags --always --dirty` |
| `make test`     | `go test ./...`                                          |
| `make test-race`| `go test -race ./...`                                    |
| `make lint`     | `golangci-lint run`                                      |
| `make clean`    | `rm -rf bin/`                                            |
| `make loadtest` | builds `bin/loadtest`                                    |
| `make bench-image` | `docker build -f deploy/Dockerfile -t wirefan:bench .` |
| `make bench`    | builds `bin/loadtest` and the `wirefan:bench` image, then runs `scripts/bench.sh` (needs Docker and bash) |
| `make release-local` | in a `golang:1.26-bookworm` container, builds `dist/wirefan_$(VERSION)_linux_amd64` and `_linux_arm64` (cgo, arm64 cross-compiled) plus `dist/SHA256SUMS`; pass `VERSION=vX.Y.Z` for release names |
| `make docs-sync`| prints reminders to keep ARCHITECTURE / DESIGN / PROTOCOL aligned |

Published release binaries come from `.github/workflows/release.yml`
instead: on a `v*` tag it runs the CI workflow, builds each arch natively,
smoke-tests `--version`, and attaches the binaries and `SHA256SUMS` to a
GitHub release.

---

## Living docs note

ARCHITECTURE.md is **navigation**, not specification. Keep it in sync:

- **Added or removed a package under `internal/`?** Update the repo map.
- **Changed the request lifecycle** (new pump, new dispatch path,
  reordered shutdown)? Update the lifecycle section and re-check the
  file:symbol landmarks.
- **Renamed or moved a function?** The file:symbol landmarks in this
  doc rot more slowly than line numbers, but they still rot;
  `make docs-sync` is a manual reminder, not an enforcement.
- **Added a new "common task"?** Add a row to "Where to look when".
- **Touching anything `docs/COMPATIBILITY.md` covers** (frames, error or
  close codes, HTTP API, flags, environment variables, metric names, the
  key database)? Within 1.x it may only be added to, never removed,
  renamed or changed in meaning. Packages under `internal/` are not
  covered.

The wire format and architectural rationale live elsewhere
(PROTOCOL.md / DESIGN.md). Don't duplicate them here.

---

## Further reading

- [`docs/DESIGN.md`](./docs/DESIGN.md): architectural decisions, alternatives considered, the *why*
- [`docs/PROTOCOL.md`](./docs/PROTOCOL.md): wire format, frame schemas, error and close codes
- [`docs/COMPATIBILITY.md`](./docs/COMPATIBILITY.md): what SemVer covers in 1.x
- [`docs/DEPLOY.md`](./docs/DEPLOY.md): production runbook for an Ubuntu 24.04 VPS
- [`docs/BENCHMARKS.md`](./docs/BENCHMARKS.md): performance numbers + methodology
- [`clients/js/README.md`](./clients/js/README.md): `@wirefan/client` usage
- `cmd/wirefan/main.go`: the canonical wiring example for every
  swappable interface in `internal/`
