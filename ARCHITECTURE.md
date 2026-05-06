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
go build ./...                  # sanity check
make build                      # -> bin/wirefan
make test                       # full unit suite
make test-race                  # race-detector pass
./bin/wirefan                   # listens on :8080, logs admin token
```

The server prints an admin Bearer token at startup
(`cmd/wirefan/main.go:49`). Use it to mint an API key:

```bash
ADMIN=...                       # paste from server log
curl -s -XPOST localhost:8080/v1/keys \
  -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"dev"}'
# -> {"id":"K-...","name":"dev","secret":"S-..."}
```

Open a WebSocket with `websocat` and play with the protocol:

```bash
KEY=K-...
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
│   ├── fanout/         Broadcast strategies (per-conn goroutine, sharded worker pool)
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
the code, with file:line landmarks.

**Boot.**

1. `cmd/wirefan/main.go:23` — `main()` sets up signal-cancelable context.
2. `cmd/wirefan/main.go:32` — `run(ctx)` wires `store`, `auth`, `metrics`,
   `registry`, `fanout`, `ratelimit`, `hub`, then constructs the server.
3. `internal/server/server.go:31` — `server.New` registers routes:
   `/v1/health`, REST under `/v1/keys` and `/v1/auth/sign`, WS at
   `/v1/connect`, `/metrics`, `/debug/pprof/*`, and the embedded demo at `/`.
4. `internal/server/server.go:54` — `Server.Run` starts `http.Server` and
   blocks on ctx; on cancel it sets the health probe to draining and
   calls `Hub.Drain` before `srv.Shutdown`.

**Connect.**

5. `internal/server/upgrade.go:76` — `UpgradeHandler.ServeHTTP` validates
   the `?key=` query, applies the per-IP phantom-conn cap
   (`upgrade.go:96`), upgrades via `coder/websocket`, mints a ULID
   `socket_id`, and hands off to `conn.Run`.
6. `internal/conn/conn.go:62` — `conn.Run` registers the conn with the
   Hub (for shutdown drain), sends the `connected` hello frame, then
   spawns `writePump` and `readPump` (`internal/conn/pumps.go`).

**Subscribe / publish.**

7. `internal/conn/pumps.go` — `readPump` reads a frame, calls `c.handle`.
8. `internal/conn/handler.go:23` — `handle` dispatches on `type`:
   - `subscribe` → `handler.go:41` — `handleSubscribe`. For
     `private-*` channels, verifies an HMAC token with
     `auth.VerifyToken`. Caps channels per conn at `defaultMaxChannelsPerConn`
     (`conn.go:51`). Calls `registry.GetOrCreate` then `hub.Subscribe`.
   - `publish` → `handler.go:71` — `handlePublish`. Rejects reserved
     `_*` channels, requires prior subscription, charges the per-key
     rate limiter, then calls `Fanout.Broadcast`.
   - `unsubscribe` → `handler.go:100` — `handleUnsubscribe`.

**Fanout.**

9. `internal/fanout/perconn.go` (default) — one goroutine per subscriber
   per Broadcast. Simple; preferred at low fan-out.
10. `internal/fanout/sharded.go` — fixed pool of N workers; tasks routed
    by hash. Used when subscriber counts are high.
11. Both call `Subscriber.Send([]byte)`. For a `Conn`, `Send` runs
    through the configured backpressure `Policy`
    (`internal/conn/policy.go:6`); on `ErrSlowConsumer` the conn is
    closed with WebSocket code 1008.

**Shutdown.**

12. SIGTERM cancels the root ctx → `Server.Run` returns, calls
    `Hub.Drain` (`internal/hub/hub.go:41`), which sends GoingAway close
    frames to every tracked conn and waits up to 30s for them to deregister.

---

## Where to look when

| Goal                                | File                                                  |
| ----------------------------------- | ----------------------------------------------------- |
| Add a new client→server message     | `internal/conn/handler.go` (extend `incoming` + `handle`) |
| Add a new HTTP route                | `internal/server/rest.go` (or `server.go:31` for top-level) |
| Tune backpressure / write a Policy  | `internal/conn/policy.go`                             |
| Tune the per-key rate limit         | `internal/ratelimit/limiter.go` (rps/burst at `cmd/wirefan/main.go:52`) |
| Add a Prometheus metric             | `internal/metrics/prom.go` (then call `Register`)     |
| Modify the wire format              | `internal/conn/handler.go` **and update `docs/PROTOCOL.md`** |
| Tweak resource limits (channels/subs)| `internal/conn/conn.go:50` constants                 |
| Change the IP phantom-conn cap      | `internal/server/upgrade.go:25` (`defaultIPCap`)      |
| Swap fanout strategy                | `cmd/wirefan/main.go:51` (`fanout.NewPerConn` ↔ sharded) |
| Swap registry impl                  | `cmd/wirefan/main.go:50` (`registry.NewSyncMap` ↔ sharded) |
| Swap store backend                  | `cmd/wirefan/main.go:34` (`store.NewMemory` ↔ sqlite) |
| Add an admin REST endpoint          | `internal/server/rest.go:23` `Register`               |
| Modify shutdown behavior            | `internal/server/server.go:54` + `internal/hub/hub.go:41` |
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
  file:line citations.
- **Renamed a file or moved a function?** The file:line citations in
  this doc will rot — `make docs-sync` is a manual reminder, not an
  enforcement.
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
