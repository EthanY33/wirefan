<p align="center">
  <img src="docs/social.png" alt="wirefan" width="100%">
</p>

# wirefan

> Single-binary Go WebSocket fanout server. Channel-based pub/sub,
> HMAC-bound subscribe tokens with replay protection, graceful drain,
> zero runtime dependencies. Goroutine-leak invariant proven under
> 1000-connection churn (`internal/server/leak_test.go`).

---

## Quickstart

```bash
git clone https://github.com/EthanY33/wirefan
cd wirefan
make build
./bin/wirefan
# Admin Bearer is printed once on stdout (and persisted to
# cmd/wirefan/var/admin.token for ops continuity).
# Open http://localhost:8080 in two tabs to see live fanout.
```

The server logs an admin Bearer token at startup. Use it to mint an API key,
then connect a WebSocket client to `ws://localhost:8080/v1/connect?key=<id>`.
Full walk-through in [`ARCHITECTURE.md`](ARCHITECTURE.md#quickstart-for-contributors).

---

## Architecture

![Architecture](docs/architecture.svg)

Two planes share one process:

- **Data plane:** `/v1/connect` upgrades to WebSocket; clients `subscribe` and
  `publish` JSON frames; `hub.Broadcast` pushes each payload to every
  subscriber on the channel.
- **Control plane:** `/v1/keys` (admin Bearer) mints API keys; `/v1/auth/sign`
  issues HMAC tokens for `private-*` channels; a separate admin listener serves
  `/metrics` (Prometheus) and `/debug/pprof/*` for production debugging.

Swappable interfaces (`Fanout`, `Registry`, `Store`, backpressure `Policy`)
mean the same wire path benchmarks two strategies. See
[`ARCHITECTURE.md`](ARCHITECTURE.md) for the repo map and request lifecycle,
and [`docs/DESIGN.md`](docs/DESIGN.md) for the rationale behind each choice.

---

## Performance

Methodology, the `Fanout × Registry` matrix runner (`scripts/bench.sh`), and
the `cmd/loadtest` driver are in place. Reproducible numbers from a 1-vCPU
ARM reference box are pending, since the project is currently undeployed. See
[`docs/BENCHMARKS.md`](docs/BENCHMARKS.md) for the methodology and target
matrix; numbers get filled in after the first hosted deploy.

What's verified today:

- Goroutine-leak invariant: 1000-connection churn under `-race` returns to
  baseline within tolerance (`internal/server/leak_test.go`).
- Per-subscriber FIFO ordering: each connection's buffered send channel
  preserves order (Go guarantees send order equals receive order). `Broadcast`
  snapshots subscribers under `RLock` then sends concurrently, so per-channel
  total ordering is intentionally not a protocol guarantee (see the rationale
  below).
- Graceful shutdown: 30 s drain, `WaitGroup`-tracked goroutines, no leaks
  (covered by `internal/server/shutdown_test.go`).

---

## Wire protocol

JSON over WebSocket, version `v1`. One JSON object per text frame.

```text
client →  GET /v1/connect?key=<API_KEY_ID>          (WS upgrade)
server →  {"type":"connected","socket_id":"01H...","version":"v1"}
client →  {"type":"subscribe","channel":"chat"}
server →  {"type":"subscribed","channel":"chat"}
client →  {"type":"publish","channel":"chat","data":{"hello":"world"}}
server →  {"type":"event","channel":"chat","data":{"hello":"world"}}   (every subscriber)
```

Full message catalog, error codes, close codes, and the HMAC flow for
`private-*` channels live in [`docs/PROTOCOL.md`](docs/PROTOCOL.md).

---

## Why these choices

- **`coder/websocket`, not `gorilla/websocket`.** The actively maintained
  successor, with a smaller, context-aware API. gorilla is in archive mode.
- **SQLite, not Postgres.** Single-file durability, zero ops. Postgres is
  out of scope for V1 (single-server deployment).
- **Concurrent broadcast, not a per-channel lock.** `Broadcast` snapshots
  subscribers under `RLock` then sends concurrently. Each connection's buffered
  channel keeps its own order, so per-subscriber FIFO holds. A per-channel
  broadcast mutex was removed because one slow consumer head-of-line-blocked the
  whole channel under the disconnect policy's write deadline.
- **HMAC channel tokens with a server-only signing secret.** No per-key crypto
  material on disk, and tokens are bound to `socket_id` so a leak can't be
  replayed on a different connection.
- **Pluggable `Fanout` and `Registry`.** The same code path benchmarks two
  strategies (per-conn goroutine vs sharded worker pool; `sync.Map` vs
  sharded RWMutex). The default ships per-conn fanout with a sync-map registry.

---

## Deferred (next steps)

- Multi-server scaling via Redis pub-sub (single-server is V1 scope)
- Message history and replay (`Last-Event-ID` style)
- Presence with join/leave diffs
- Polished client SDK (raw WebSocket only for now)

---

## Build and test

```bash
make build      # -> bin/wirefan
make test       # full unit suite
make test-race  # race-detector pass
make lint       # golangci-lint
make bench      # full benchmark matrix (requires Linux + bash; see scripts/bench.sh)
```

`make bench` drives `cmd/loadtest` against a local server across the
`Fanout × Registry` matrix. POSIX-only because of the shell driver; the
underlying binaries build and run on every Go target.

---

## Docs

- [`ARCHITECTURE.md`](ARCHITECTURE.md): repo map, request lifecycle, where to look when
- [`docs/DESIGN.md`](docs/DESIGN.md): architectural decisions, alternatives, scaling roadmap
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md): wire format spec, frame schemas, error codes
- [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md): methodology and results

---

## License

MIT. See [`LICENSE`](LICENSE).
