# `wirefan` — Design Specification

**Project**: `wirefan` — single-binary Go WebSocket fanout server
**Author**: Ethan Yucetepe
**Date**: 2026-05-04
**Status**: Pre-implementation design, awaiting user review

> Working repo location: `C:\Users\ethan\Desktop\relay\` (kept as the on-disk dir; the GitHub repo and binary will be named `wirefan`). If user picks `socketrelay` or sticks with `relay`, replace globally before implementation.

---

## Purpose

`wirefan` is a portfolio-quality real-time fanout server, built primarily to serve as the centerpiece engineering artifact in a Big Tech new-grad SWE application portfolio (target companies: Datadog, Stripe, Google, etc.). It is intended to demonstrate idiomatic Go backend engineering: WebSocket-based pub/sub, careful concurrency, graceful shutdown, instrumented performance characteristics, and reproducible benchmarks.

Functionally, `wirefan` is a generic fanout server: clients connect, authenticate, subscribe to named channels, and broadcast/receive opaque messages on those channels. The server does not interpret payload content. The model is roughly Pusher / Centrifugo lite, with deliberate scope reduction.

## Decisions to lock before implementation

These are user-decision blockers — implementation should not start until each is resolved:

1. **Hosting target ($0 ongoing budget).** The live demo must stay up for months during the job-hunt at zero ongoing cost.
   - **Primary: Oracle Cloud Always Free (Ampere A1).** 4 ARM vCPUs / 24 GB RAM permanently free, supports anything, never expires. CC required for sign-up verification but never charged. Strongest engineering signal of the free options.
   - **Backup: Cloudflare Tunnel from an always-on home machine (desktop / Pi / NAS).** $0 if hardware exists, public URL via `cloudflared`, real engineering signal. Demo availability tied to that machine's uptime.
   - **Fallback: Fly.io with $5 trial credit.** ~6 weeks runway before CC needed; useful if Oracle sign-up stalls. Migrate to Oracle once approved.
   - Rejected: Render free (idle-sleep kills WS), Railway (per-usage credit burn), AWS/GCP free tiers (12-mo expiry on AWS; GCP e2-micro is too small).
2. **Time budget.** Locked-in scope is ~40–50 focused hours of evening work. New-to-Go would roughly double that. Realistic target: **3 weeks of evenings** (1–2 hours/day) with 2 weeks as a stretch.
3. **Repo name.** `relay` collides with Facebook's GraphQL client; portfolio reads cleaner with a distinctive name. Verified availability on `EthanY33`: all candidates are free. Recommended: **`wirefan`** (distinctive, memorable, no major OSS collisions) or **`socketrelay`** (most descriptive, zero exact-name matches anywhere on GitHub). Spec assumes `wirefan` unless redirected.

## Scope

### Locked-in features

1. WebSocket server using `coder/websocket` (formerly `nhooyr.io/websocket`); JSON wire protocol documented in `PROTOCOL.md`.
2. Channel-based publish/subscribe; server does not interpret message bodies.
3. API-key auth via REST control plane; HMAC-SHA256–signed channel tokens for `private-*` channels, **bound to the connection's `socket_id`** (Pusher pattern, prevents token replay across connections).
4. Per-connection buffered `send` chan with **configurable slow-consumer policy** (`disconnect` | `drop-oldest` | `drop-newest`).
5. Heartbeat: 30s ping interval, 60s pong timeout. Read deadline reset on any inbound message.
6. Graceful shutdown: connection drain (30s grace), full ctx cancellation, WaitGroup-tracked goroutines, **zero-leak verified by test**.
7. Observability: `slog` structured logs + `/metrics` (Prometheus) + `/debug/pprof`. **`--otel-endpoint=` flag** wired with optional exporter — dormant when flag empty, recruiter-readable as "instrumented for tracing."
8. Resource limits (all flag-configurable): `--max-channels-per-conn=64`, `--max-subscribers-per-channel=10000`, `--max-frame-bytes=65536`, per-API-key publish rate limit (token bucket, default 100 msg/s, burst 200).
9. Pluggable interfaces, both implementations of each shipped and benchmarked:
   - `Fanout`: `PerConnFanout` (per-connection buffered chan) vs `ShardedPoolFanout` (NumCPU×2 worker pool drawing from sharded queues). **Default: `--fanout=per-conn`** (classic Go pattern, simpler reasoning). Switchable via flag.
   - `Registry`: `SyncMapRegistry` (sync.Map) vs `ShardedMapRegistry` (16-shard `RWMutex+map`). **Default: `--registry=sync-map`** (simpler default; sharded selected only when benchmarks justify it). Switchable via flag.
   - `Store`: `sqliteStore` (default, durable) vs `memoryStore` (tests, ephemeral deploy mode). Selected via `--store=sqlite|memory`.
10. `_wirefan-stats` reserved system channel — server publishes its own connection count / msg/s / drop count to the channel; clients with valid API key may *subscribe* (read-only). Demo client subscribes to it: the system dogfoods itself, recruiter sees live numbers arriving via the same fanout being demonstrated.
11. Browser demo client at `/`: vanilla JS, three panels (connection, messages, live-stats), capped stress-test button (50 phantom connections per browser tab, 10-second time bound, server-side cap of 200 phantom connections per source IP).
12. Single-binary deploy: multi-stage Dockerfile, `fly.toml`, persistent volume for `wirefan.db`.

### Deliberately deferred

Mentioned as "next steps" in `DESIGN.md`, with the design sketch:

- **Multi-server scaling via Redis pub-sub.** Sticky-vs-stateless decisions, deduplication, fan-in ordering, cluster failover — easily a week alone.
- **Message history / replay on reconnect.** Requires a per-channel ring buffer, message IDs, gap detection, `Last-Event-ID`-style protocol.
- **Presence with join/leave diffs.** Phoenix-Channels-style; CRDT or naive O(N) — both have real gotchas.
- **Polished client SDK** with auto-reconnect, exponential backoff, subscription replay, in-flight queue. Itself a 1-week project; raw browser demo only for now.

### Distinctive engineering artifact

A reproducible **benchmark report** comparing two fanout strategies on identical hardware:

- `PerConnFanout` vs `ShardedPoolFanout`
- Test matrix: 10k / 25k / 50k connections × 100 / 1000 channels × 10 / 100 msg/s/conn
- Captured: p50 / p99 / p999 broadcast latency (publisher-write → last-subscriber-receive), CPU%, RSS, goroutine count
- Output: `BENCHMARKS.md` with tables, line-chart PNGs, pprof CPU + heap flamegraphs in `docs/profiles/`, declared winner with hot-path explanation
- Separate set of benches for `SyncMapRegistry` vs `ShardedMapRegistry` on the channel-lookup hot path
- `make bench` reproduces the runs end-to-end

## Architecture

### Repository layout

```
wirefan/
├── cmd/
│   ├── wirefan/      # main binary entrypoint
│   └── loadtest/     # standalone WS client load generator
├── internal/
│   ├── server/       # HTTP routing, WS upgrade handler
│   ├── hub/          # central channel registry + Channel struct
│   ├── fanout/       # Fanout interface + two implementations
│   ├── registry/     # Registry interface + two implementations
│   ├── auth/         # API keys, HMAC channel tokens, socket_id binding
│   ├── store/        # Store interface + sqliteStore + memoryStore
│   ├── ratelimit/    # token-bucket per-key publish limiter
│   └── metrics/      # Prometheus collectors + OTel hook
├── web/              # embedded demo client (HTML + JS via embed.FS)
├── docs/             # DESIGN.md, PROTOCOL.md, BENCHMARKS.md, profiles/
├── Dockerfile
├── fly.toml
├── Makefile
└── go.mod
```

### Core components

- **Hub** — owns the `Registry` (which holds `Channel`s by name). Coordinates subscribe / unsubscribe / lookup. Hot-path methods are wrapped behind the Registry interface; the chosen implementation is configured at boot via flag.
- **Channel** — subscriber set + per-channel mutex. Broadcast iteration locks the channel, which serializes broadcasts on a single channel and preserves FIFO ordering from any single publisher. Self-cleans when subscriber count hits 0.
- **Connection** — wraps a single WS conn:
  - read goroutine: pulls frames, parses JSON, dispatches by type
  - write goroutine: pulls from `send chan []byte` (size 64), writes with deadline, owns the ping ticker
  - both tracked via Hub's WaitGroup
- **Fanout** — interface:
  ```go
  type Fanout interface {
      Broadcast(ctx context.Context, channel string, msg []byte)
  }
  ```
  - `PerConnFanout`: lookup channel, iterate subscribers, push onto each conn's `send` chan; backpressure policy resolved per-conn.
  - `ShardedPoolFanout`: enqueue broadcast job onto a per-channel-shard worker queue; worker pool drains queues, iterates subscribers, applies policy.
- **Auth** — Bearer API key for REST endpoints. `socket_id` (ULID) assigned at upgrade and sent in `connected` message. HMAC-SHA256 signs `socket_id|channel`; tokens expire 5 min after issuance.
- **Store** — interface:
  ```go
  type Store interface {
      CreateKey(ctx, name) (Key, error)
      LookupKey(ctx, keyID) (Key, error)
      RevokeKey(ctx, keyID) error
      ListKeys(ctx) ([]Key, error)
  }
  ```
  Default: `sqliteStore` (single file, durable). `memoryStore` for tests + ephemeral-deploy mode.

## Wire protocol

JSON over WebSocket. Documented in `PROTOCOL.md`. Versioned via `version` field in the `connected` message (current: `"v1"`).

### Message shapes

```jsonc
// Server → Client (sent immediately after upgrade)
{"type": "connected", "socket_id": "01HZ...", "version": "v1"}

// Client → Server
{"type": "subscribe",   "channel": "chat-1", "token": "<HMAC>"}  // token only for "private-*"
{"type": "unsubscribe", "channel": "chat-1"}
{"type": "publish",     "channel": "chat-1", "data": {...}}

// Server → Client
{"type": "subscribed",   "channel": "chat-1"}
{"type": "unsubscribed", "channel": "chat-1"}
{"type": "event",        "channel": "chat-1", "data": {...}, "id": "01HZ..."}
{"type": "error",        "code": "AUTH_FAILED", "message": "..."}
```

### Channel naming

- `public-*` or unprefixed: any client with valid API key may subscribe.
- `private-*`: requires server-signed HMAC token bound to the subscriber's `socket_id`.
- Reserved: `_wirefan-stats` (read-only for clients; only the server publishes to it).

### Limits

- Max frame size: 64 KB
- Max channels per connection: 64
- Max subscribers per channel: 10,000
- Publish rate limit: 100 msg/s, burst 200, per API key

### Ordering guarantee

FIFO is preserved for messages from any single publisher to any single subscriber on a single channel (enforced by the per-channel broadcast mutex). Cross-publisher ordering is **not** guaranteed. Cross-channel ordering is **not** guaranteed.

### Backpressure policies

Applied to per-connection `send` chan (default capacity 64). Default policy is `disconnect`; configurable globally via flag and per-channel via REST control-plane attribute.

- `disconnect` — close 1008 with reason `"slow_consumer"` on full chan
- `drop-oldest` — pop oldest message from chan, push new
- `drop-newest` — drop incoming msg, log, increment metric

## HTTP API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/` | none | embedded demo client |
| GET | `/v1/connect?key=<api_key>` | API key (query string) | WebSocket upgrade |
| POST | `/v1/auth/sign` | API secret (Bearer) | issue HMAC token; body: `{socket_id, channel}` |
| POST | `/v1/keys` | admin Bearer | create API key |
| GET | `/v1/keys` | admin Bearer | list keys |
| DELETE | `/v1/keys/:id` | admin Bearer | revoke key |
| GET | `/v1/health` | none | liveness; **returns 503 during shutdown drain** |
| GET | `/metrics` | none | Prometheus exposition |
| GET | `/debug/pprof/*` | localhost-only | pprof endpoints |

### Origin allowlist

`--allowed-origins=` flag (default `*` for the public demo). DESIGN.md flags this as a production hardening.

### API-key-as-query-string tradeoff

Query-string credentials show up in proxy access logs. `wirefan` accepts the tradeoff (Pusher does too) and documents the alternative in DESIGN.md: `Sec-WebSocket-Protocol`-as-bearer is browser-compatible and considered, but rejected for protocol simplicity at this scope.

### Auth flow

1. Operator boots `wirefan --admin-token-print` → admin Bearer printed once on stdout.
2. Operator creates API keys via REST → returns `key_id` + `secret`.
3. Application server (the server's customer) holds the `secret`, calls `POST /v1/auth/sign` with `{socket_id, channel}` to issue HMAC tokens for browser clients subscribing to private channels.
4. Browser client connects WS with `?key=<key_id>`, receives `connected` with `socket_id`, then sends `subscribe` for the private channel including the HMAC token from step 3.

## Data flow

### Publish path

1. Client sends `{type:"publish", channel:"X", data:{...}}` over WS
2. Read goroutine parses JSON, validates type
3. Auth check: connection must be subscribed to the channel
4. Rate-limit check: token bucket for the API key; on exhaustion, send `error{code:"RATE_LIMITED"}` and drop
5. Generate ULID msg ID; wrap as `event` message
6. `Registry.Lookup(channel)` → `*Channel`
7. `Channel.Broadcast(msg)` invokes the configured Fanout impl
8. Each subscriber's `send` chan receives; write goroutine flushes per its deadline
9. If a subscriber's chan is full, the configured backpressure policy applies

### Connection lifecycle

1. WS upgrade at `/v1/connect?key=...`
2. Validate API key + check origin allowlist → upgrade or 401/403
3. Assign `socket_id` (ULID); start write goroutine; enqueue `connected` message
4. Start read goroutine; start ping ticker
5. Process `subscribe` / `unsubscribe` / `publish` until close
6. On close: remove from all channel subscriber sets, close send chan, write goroutine drains pending then exits, read goroutine exits, WaitGroup decrements

### Shutdown sequence

1. SIGTERM → root ctx canceled
2. HTTP listener stops accepting new connections; `/v1/connect` returns 503
3. `/v1/health` returns 503 (load balancer deroutes)
4. All connections sent close-frame 1001; 30s grace
5. WaitGroup waits for all goroutines; force-close stragglers after grace
6. Hub clears state; SQLite closes; exit 0

## Error handling

### WS error class table

| Trigger | Server response |
|---|---|
| Bad API key on upgrade | HTTP 401, no upgrade |
| Origin denied | HTTP 403, no upgrade |
| Bad HMAC on private subscribe | `error{code:"AUTH_FAILED"}`, **don't close** |
| Publish to non-subscribed channel | `error{code:"NOT_SUBSCRIBED"}`, don't close |
| Publish rate-limited | `error{code:"RATE_LIMITED"}`, don't close |
| Duplicate subscribe | reply `subscribed` (idempotent no-op), don't close |
| Malformed JSON frame | `error` then close 1003 (Unsupported Data) |
| Frame > 64 KB | close 1009 (Message Too Big) |
| Send chan full + policy=disconnect | close 1008 (Policy Violation), reason `"slow_consumer"` |
| Pong timeout (60s) | close 1001 (Going Away) |
| Server shutdown | close 1001 with 30s drain |

### Operability metrics (Prometheus)

- `wirefan_connections_total` — gauge, current open connections
- `wirefan_channels_total` — gauge, current active channels
- `wirefan_messages_published_total` — counter
- `wirefan_messages_dropped_total{reason}` — counter (`reason` ∈ `slow_consumer` | `rate_limit` | `oversize`)
- `wirefan_broadcast_latency_seconds` — histogram (p50/p99/p999)
- `wirefan_upgrade_rejected_total{reason}` — counter (`bad_key` | `bad_origin` | `phantom_cap`)
- `wirefan_auth_failures_total` — counter

## Testing

### Layers

| Layer | Tools | What |
|---|---|---|
| Unit | stdlib + testify, `-race` always on | Hub subscribe/unsubscribe/broadcast under concurrency; auth HMAC roundtrip; rate-limit; both Store impls; both Fanout impls; both Registry impls |
| Integration | `httptest` + `coder/websocket` test client | subscribe→publish→receive; private-channel HMAC flow; multi-subscriber fanout; reconnect cleanup; shutdown drain |
| Concurrency proof | stdlib | **Headline test**: 1k connection-churn cycles → shutdown → assert `runtime.NumGoroutine()` returns to baseline within 1s |
| Stress | custom | Parallel subscribe/unsubscribe at 10k QPS; per-publisher monotonic-ID ordering verifier; backpressure-policy enforcement verifier |

### Test discipline

- All tests pass under `-race`. No exceptions.
- Goroutine-leak test runs in CI as a regular unit test.
- No `time.Sleep` — use `synctest` (Go 1.25 stdlib) or channel-based synchronization.
- Coverage gate: 70% on internal packages.

### Benchmark methodology

`cmd/loadtest/` Go binary; `make bench` reproduces. Matrix: 10k / 25k / 50k connections × 100 / 1000 channels × 10 / 100 msg/s/conn. Hardware: 1-vCPU Fly machine (or equivalent local Docker). Captured: p50 / p99 / p999 broadcast latency, CPU%, RSS, goroutine count. Written to `BENCHMARKS.md`.

### Stretch: Centrifugo head-to-head

`make bench-compare` runs the same matrix against `centrifugo` and emits a side-by-side comparison. Marked stretch — pursue only if primary scope ships ahead of schedule. Bench harness must support pluggable target.

## Tooling & Quality Gates

Implementation must use every applicable Claude Code skill. This is not optional — the skills exist to prevent the "tutorial-grade" pattern recruiters filter on. Each phase has a required skill set; nothing ships without passing every applicable gate.

### Mandatory skill invocations by phase

| Phase | Skill | Purpose |
|---|---|---|
| Plan generation | `superpowers:writing-plans` | Break this spec into ordered, TDD-gated implementation tasks |
| Per-task execution | `superpowers:test-driven-development` | Tests precede implementation for every task |
| Architecture writeup | `supermind:sm-init` then `living-docs` | Generate `ARCHITECTURE.md` and keep it in sync after every code change |
| Demo client design | `frontend-design` + `ui-ux-pro-max` | The `/` page must be distinctive — no AI-generic dark-mode-with-blue-accents look |
| README hero & layout | `frontend-design` + `ui-ux-pro-max` | Treat the README as a landing page for the project; design accordingly |
| Repo social-preview card | `frontend-design` + `screenshot-tripwire` | The OG card recruiters see when the repo is shared on LinkedIn / X |
| Architecture diagram | `frontend-design` (SVG) | Committed `docs/architecture.svg`, not a hand-drawn whiteboard photo |
| README copy | `copy-tripwire` | Catch AI-default tells before publishing |
| Screenshots / banner art | `screenshot-tripwire` | Validate dimensions, no transparent corners, no monochrome flat palette |
| Demo screencast (.gif/.mp4) | `trailer-tripwire` | Pattern-check the screencast before embedding in README |
| Implementation review | `pr-review-toolkit:code-reviewer` | Architectural review against SOLID, idiomatic Go, testability |
| Type design | `pr-review-toolkit:type-design-analyzer` | Run on every new type added in `internal/` |
| Error handling | `pr-review-toolkit:silent-failure-hunter` | Run on every PR / merge that touches `catch` / `if err != nil` paths |
| Test coverage thoroughness | `pr-review-toolkit:pr-test-analyzer` | Required before declaring any feature "done" |
| Comment audit | `pr-review-toolkit:comment-analyzer` | After writing godocs / inline comments |
| Code simplification | `pr-review-toolkit:code-simplifier` | After completing any logical chunk |
| CLAUDE.md authoring | `claude-md-management:revise-claude-md` | Once the project codebase exists; document conventions for future sessions |
| Skill usage enforcement | `superpowers:using-superpowers` | Auto-loaded at session start to enforce skill discipline |

### Skipped (with rationale)

- `atelier` — goneIdle-specific brand source-of-truth. `wirefan` is a clean-break project; introducing atelier would require a separate brand kit, overkill for 2 weeks. Direct `frontend-design` + `ui-ux-pro-max` calls cover the same ground.
- `linkedin-post`, `steam-announcement`, `patch-notes`, `changelog` — goneIdle/TideWane-specific.
- `webview2-debug`, `gameplay-trailer`, etc. — game-specific.

### Quality gates (each must pass before commit)

1. All tests pass under `-race`. No exceptions.
2. `golangci-lint run` clean.
3. `code-reviewer` agent reports zero remaining issues. Re-run after fixes until clean.
4. `silent-failure-hunter` reports zero unaddressed findings on changed error-handling code.
5. For UI / README / asset changes: `ui-ux-pro-max` review pass + `screenshot-tripwire` clean + `copy-tripwire` clean.
6. For doc changes affecting architecture: `living-docs` rerun to keep `ARCHITECTURE.md` in sync.
7. Goroutine-leak unit test passes (`runtime.NumGoroutine()` returns to baseline).

### Treat the GitHub repo as a designed product

The repo is a portfolio asset, not a code dump. Every recruiter-visible surface — README hero, architecture diagram, demo client, social-preview OG card, even the commit message style — gets the same design discipline as the code. The skills above are the enforcement mechanism: don't ship a recruiter-visible artifact without invoking the relevant skill.

## Deployment

### Oracle Cloud Always Free (default plan, $0)

- Provision Ampere A1 instance (1 OCPU / 6 GB minimum free allocation; up to 4 OCPU / 24 GB available)
- Multi-stage Dockerfile: `golang:1.25-alpine` builder → `gcr.io/distroless/static`, ARM64 image
- Run via `systemd` unit (`/etc/systemd/system/wirefan.service`) for restart-on-crash + boot-start
- Secrets via systemd `EnvironmentFile=` pointing at a 0600-permissioned `.env`
- TLS via Caddy reverse proxy (auto-Let's Encrypt) listening on `:443`, proxying to local `wirefan` on `:8080`
- Domain: `wirefan.ethanyucetepe.dev` (free Cloudflare DNS — user already owns the apex per memory) OR a free `*.duckdns.org` subdomain
- Cost: $0/mo permanently

### Backup: Cloudflare Tunnel from a home machine

- `cloudflared tunnel` runs on user's always-on machine, exposes local `wirefan` to a public URL
- Zero infra to manage, public TLS automatic
- Tradeoff: demo availability tracks the home machine's uptime

### Fallback: Fly.io trial ($5 credit, ~6 weeks)

- Same Dockerfile, `fly.toml` with `auto_stop_machines=false`
- Migrate to Oracle once Always Free is approved

### Alternatives explicitly rejected

- Render free (idle-sleep kills WS)
- Railway free credit (per-usage credit burn for always-on)
- AWS Free Tier (12-mo expiry)
- GCP e2-micro (too small; only certain regions)

### Demo client polish

- `/` serves a single embedded HTML page (vanilla JS + WebSocket API, no framework)
- Three panels: connection (status + socket_id + channels), messages (live log + send box), live-stats (subscribed to `_wirefan-stats`)
- "Stress test" button:
  - Cap: 50 phantom connections per browser tab
  - Time bound: 10 seconds, then auto-disconnect
  - Server-side cap: 200 phantom connections per source IP (further upgrades rejected with HTTP 429)
- "Open two tabs to see fanout" instructions visible above the fold

## README structure

```
# wirefan

[Live demo: https://wirefan.ethanyucetepe.dev]
[30-second screencast.gif inline]

> Single-binary Go WebSocket fanout server.
> 50k concurrent connections, p99 broadcast latency 4 ms
> on a 1-vCPU machine. Zero runtime dependencies.

## Quickstart
$ docker run -p 8080:8080 ethany33/wirefan

## Architecture       [diagram.svg]
## Performance        [headline table from BENCHMARKS.md + flamegraph link]
## Wire protocol      [→ PROTOCOL.md]
## Why these choices  [3-5 design decisions w/ one-line justifications]
## Deferred           [next-step list]
## Build & test       [Make targets]
```

### Other docs

- `DESIGN.md` — full architecture, fanout-strategy comparison, registry-primitive comparison, alternatives considered (custom epoll loop, Redis multi-server, etc.), open questions, future work
- `PROTOCOL.md` — wire-format spec, auth flow, error codes, message-size + rate limits, ordering semantics
- `BENCHMARKS.md` — methodology, results, charts, pprof links, declared winner + hot-path rationale
- `CONTRIBUTING.md` — short, dev setup only
- `LICENSE` — MIT

## GitHub repo settings

- Description: "Single-binary WebSocket fanout server in Go"
- Topics: `websocket`, `golang`, `real-time`, `pubsub`, `fanout-server`
- README first 3 lines = live demo URL + screencast GIF + headline performance number (the 5-second-test pass)
- Pin on EthanY33 profile: replaces one of the current 5 (decide closer to ship — most likely `wirefan` takes top-left, `news-bias-analyzer` takes another slot, drop one tripwire)

## Stretch goals

1. **Centrifugo head-to-head perf comparison** in `BENCHMARKS.md` — 2–3 days, only if primary scope ships ahead
2. **OTel exporter active** (currently flag-wired but dormant) — flip on when implementation buffer permits
3. **Custom domain** (`wirefan.ethanyucetepe.dev` or similar) — polish step

## Out of scope

Explicit non-goals (referenced repeatedly to prevent scope creep):

- Multi-server scaling (Redis pub-sub, etc.)
- Message history / replay
- Presence with join/leave diffs
- Client-side SDK (raw WS only)
- WebTransport / HTTP/3 transport
- MessagePack / Protobuf wire formats
- Custom epoll/kqueue event loop (gnet, nbio)
- Pusher-protocol drop-in compatibility
- Exactly-once / ordered delivery across reconnects

## Open questions

- **Hosting confirmation**: spec defaults to Oracle Cloud Always Free; user confirms or picks an alternative (Cloudflare Tunnel from home / Fly.io trial)
- **Repo name confirmation**: spec defaults to `wirefan`; user confirms or picks `socketrelay` (also free, more descriptive)
- **Time budget**: 2 weeks tight, 3 weeks comfortable — user picks the realistic target
- **Pin slot strategy**: which of the current 5 to drop when `wirefan` and `news-bias-analyzer` are pinned — defer until ship
