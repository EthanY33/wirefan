# wirefan — Architecture

> Developer onboarding for **navigating the code**. For *why* decisions
> were made, see [`docs/DESIGN.md`](./docs/DESIGN.md). For the on-the-wire
> contract, see [`docs/PROTOCOL.md`](./docs/PROTOCOL.md). For perf
> numbers, see [`docs/BENCHMARKS.md`](./docs/BENCHMARKS.md).
>
> This file answers: "I just cloned the repo — where do I look?"

---

## Quickstart for contributors

```bash
git clone https://github.com/EthanY33/wirefan && cd wirefan
go build ./...                            # sanity check (CGO required: sqlite driver)
make build                                # -> bin/wirefan
make test                                 # full unit suite
make test-race                            # race-detector pass
./bin/wirefan --dev --allowed-origins='*' # public :8080, admin 127.0.0.1:6060
```

`--allowed-origins` is required; `'*'` is only accepted together with
`--dev`. The admin Bearer token is never printed: on first boot it is
written to `var/admin.token` (or `$WIREFAN_STATE_DIR/admin.token`) and
reused on later boots; see `cmd/wirefan/main.go: resolveAdminToken`.
Use it to mint an API key against the admin listener:

```bash
ADMIN=$(cat var/admin.token)
curl -s -XPOST http://127.0.0.1:6060/v1/keys \
  -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"dev"}'
# -> {"id":"01K...","name":"dev","secret":"..."}   (id/secret are hex+ULID, shown once)
```

Open a WebSocket with `websocat` and play with the protocol:

```bash
KEY=01K...                      # the "id" field from the mint response
websocat "ws://localhost:8080/v1/connect?key=$KEY"
> {"type":"subscribe","channel":"chat"}
> {"type":"publish","channel":"chat","data":{"hello":"world"}}
```

The bundled demo client lives at `http://localhost:8080/` (served from
`web/embed.go`).

---

## Repo map

```
wirefan/
├── cmd/
│   ├── wirefan/        main entry: ctx wiring, signal handling, run()
│   └── loadtest/       standalone load generator (cmd/loadtest/main.go)
├── internal/
│   ├── auth/           API-key secret hashing + HMAC channel tokens
│   ├── conn/           WS Conn lifecycle: pumps, message router, backpressure policies
│   ├── fanout/         Broadcast strategies (inline per-conn, sharded worker pool)
│   ├── hub/            Process-wide conn set; drains close frames on shutdown
│   ├── metrics/        Prometheus collectors + OTel hook (dormant by default)
│   ├── ratelimit/      Per-key token-bucket limiter w/ background GC
│   ├── registry/       name → *Channel map (sync.Map and sharded impls)
│   ├── server/         HTTP mux, /v1/health, REST handlers, WS upgrade handler
│   └── store/          API-key persistence (memory + sqlite)
├── web/                Embedded demo client (HTML/JS/CSS via go:embed)
├── docs/               DESIGN.md, PROTOCOL.md, BENCHMARKS.md
├── scripts/bench.sh    Benchmark driver (used by `make bench`)
└── Makefile            build / test / test-race / lint / loadtest / bench / docs-sync
```

Top-level files of note:

- `go.mod` — module path is `github.com/EthanY33/wirefan`.
- `Makefile` — every contributor command is in here; if you find yourself
  typing a long `go ...` invocation more than once, add a target.
- `LICENSE` — MIT.

---

## Request lifecycle

A client connects, subscribes, then publishes. Here is the path through
the code, with file:symbol landmarks (symbols survive refactors; line
numbers do not).

**Boot.**

1. `cmd/wirefan/main.go: main` — sets up the signal-cancelable context.
2. `cmd/wirefan/main.go: run` — wires `store`, `auth`, `metrics`,
   `registry`, `fanout`, `ratelimit`, `hub`, then constructs the server.
3. `internal/server/server.go: New` — registers routes. Public mux:
   `/v1/health`, `/v1/auth/sign`, WS at `/v1/connect`, and the embedded
   demo at `/`. Admin mux (separate listener, `--admin-addr`, default
   `127.0.0.1:6060`): `/v1/keys`, `/metrics`, `/debug/pprof/*`.
4. `internal/server/server.go: Server.Run` — starts both `http.Server`s
   and blocks on ctx; on cancel it sets the health probe to draining and
   calls `Hub.Drain` before `srv.Shutdown`.

**Connect.**

5. `internal/server/upgrade.go: UpgradeHandler.ServeHTTP` — validates
   the `?key=` query, applies the per-IP phantom-conn cap
   (`defaultIPCap`, overridable via `WIREFAN_IP_CAP`), upgrades via
   `coder/websocket`, mints a ULID `socket_id`, and hands off to
   `conn.Run`.
6. `internal/conn/conn.go: Run` — registers the conn with the
   Hub (for shutdown drain), sends the `connected` hello frame, then
   spawns `writePump` and `readPump` (`internal/conn/pumps.go`).

**Subscribe / publish.**

7. `internal/conn/pumps.go` — `readPump` reads a frame, calls `c.handle`.
8. `internal/conn/handler.go: handle` — dispatches on `type`:
   - `subscribe` → `handler.go: handleSubscribe`. For
     `private-*` channels, verifies a socket-bound HMAC token with
     `auth.VerifyTokenAgainst` (jti replay cache). Caps channels per
     conn at `defaultMaxChannelsPerConn` (`internal/conn/conn.go`).
     Calls `registry.GetOrCreate` then `hub.Subscribe`.
   - `publish` → `handler.go: handlePublish`. Rejects reserved
     `_*` channels, requires prior subscription, charges the per-key
     rate limiter, then calls `Fanout.Broadcast`.
   - `unsubscribe` → `handler.go: handleUnsubscribe`.

**Fanout.**

9. `internal/fanout/perconn.go` (default) — inline `hub.Broadcast` on
   the publisher's own read goroutine. Zero extra hops.
10. `internal/fanout/sharded.go` — fixed pool of N workers; broadcasts
    routed by channel-name hash so hot channels overlap across cores.
11. Both call `Subscriber.Send([]byte)`. For a `Conn`, `Send` runs
    through the configured backpressure `Policy`
    (`internal/conn/policy.go: Policy`); on `ErrSlowConsumer` the conn
    is closed with WebSocket code 1008.

**Shutdown.**

12. SIGTERM cancels the root ctx → `Server.Run` returns, calls
    `Hub.Drain` (`internal/hub/hub.go: Drain`), which sends GoingAway
    close frames to every tracked conn and waits up to 30s for them to
    deregister.

---

## Where to look when

| Goal                                | File                                                  |
| ----------------------------------- | ----------------------------------------------------- |
| Add a new client→server message     | `internal/conn/handler.go` (extend `incoming` + `handle`) |
| Add a new HTTP route                | `internal/server/rest.go` (or `server.go: New` for top-level) |
| Tune backpressure / write a Policy  | `internal/conn/policy.go`                             |
| Tune the per-key rate limit         | `internal/ratelimit/limiter.go` (rps/burst at `cmd/wirefan/main.go: run`) |
| Add a Prometheus metric             | `internal/metrics/prom.go` (then call `Register`)     |
| Modify the wire format              | `internal/conn/handler.go` **and update `docs/PROTOCOL.md`** |
| Tweak resource limits (channels/subs)| `internal/conn/conn.go` constants                    |
| Change the IP phantom-conn cap      | `WIREFAN_IP_CAP` env (`internal/server/upgrade.go: defaultIPCap`) |
| Swap fanout strategy                | `--fanout=per-conn\|sharded` (`cmd/wirefan/main.go: run`) |
| Swap registry impl                  | `--registry=sync-map\|sharded` (`cmd/wirefan/main.go: run`) |
| Swap store backend                  | `--store=sqlite\|memory`, `--db-path` (`cmd/wirefan/main.go: run`) |
| Add an admin REST endpoint          | `internal/server/rest.go: RestHandler.RegisterAdmin`  |
| Modify shutdown behavior            | `internal/server/server.go: Server.Run` + `internal/hub/hub.go: Drain` |
| Sign HMAC channel tokens            | `internal/auth/token.go`                              |
| Edit the demo client UI             | `web/index.html`, `web/client.js`, `web/styles.css`   |

---

## Test layout

Tests live next to the code they cover (`*_test.go`). Highlights:

- `internal/server/leak_test.go` — goroutine-leak proof: opens/closes
  many WS conns, then asserts `runtime.NumGoroutine` returns to baseline.
  Run alone:
  ```bash
  go test -run TestNoGoroutineLeakAfterChurn ./internal/server -v
  ```
- `internal/server/shutdown_test.go` — graceful drain, `/v1/health` 503-on-drain.
- `internal/conn/handler_test.go` — protocol message dispatch.
- `internal/conn/policy_test.go` — backpressure policies in isolation.
- `internal/fanout/*_test.go` — both fanout impls hit the same suite.
- `internal/registry/*_test.go` — both registry impls hit the same suite.
- `internal/store/*_test.go` — memory + sqlite both satisfy `Store`.
- `internal/ratelimit/limiter_test.go` — bucket math + GC eviction.
- `internal/auth/{keys,token}_test.go` — secret hashing + HMAC tokens.

Run subsets:

```bash
make test                         # everything
make test-race                    # everything, -race
go test ./internal/conn/...       # one package tree
go test -run TestSubscribe ./...  # by name
```

---

## Build artifacts

Both binaries land in `bin/`:

- `bin/wirefan` — the server. Built by `make build`.
- `bin/loadtest` — the load generator. Built by `make loadtest`.

Make targets:

| Target          | What it does                                             |
| --------------- | -------------------------------------------------------- |
| `make build`    | `go build -o bin/wirefan ./cmd/wirefan`                  |
| `make test`     | `go test ./...`                                          |
| `make test-race`| `go test -race ./...`                                    |
| `make lint`     | `golangci-lint run`                                      |
| `make clean`    | `rm -rf bin/`                                            |
| `make loadtest` | builds `bin/loadtest`                                    |
| `make bench`    | builds both, then runs `scripts/bench.sh`                |
| `make docs-sync`| prints reminders to keep ARCHITECTURE / DESIGN / PROTOCOL aligned |

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

The wire format and architectural rationale live elsewhere
(PROTOCOL.md / DESIGN.md). Don't duplicate them here.

---

## Further reading

- [`docs/DESIGN.md`](./docs/DESIGN.md) — architectural decisions, alternatives considered, the *why*
- [`docs/PROTOCOL.md`](./docs/PROTOCOL.md) — wire format, frame schemas, error codes
- [`docs/BENCHMARKS.md`](./docs/BENCHMARKS.md) — performance numbers + methodology
- `cmd/wirefan/main.go` — the canonical wiring example for every
  swappable interface in `internal/`
