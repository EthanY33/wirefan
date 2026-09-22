# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
From 1.0.0 on, [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md) defines
exactly what that covers.

## [Unreleased]

## [1.0.0] - 2026-09-22

1.0.0 is the stability release. The wire protocol (`v1`), the HTTP API,
the command-line flags, the environment variables, the metric names and the
on-disk key database are now covered by Semantic Versioning, as listed in
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md). Getting there meant a
release pipeline, a client library, an operations story, and an audit of
the whole codebase that found and fixed two serious bugs before they could
reach a 1.x user: healthy idle connections were dropped every 60 seconds,
and a crafted `socket_id` could stretch a subscribe token to a second
private channel.

### Security

- Subscribe tokens could be made to verify for a different private channel.
  The MAC input joined its fields with `|` without escaping and
  `POST /v1/auth/sign` did not validate `socket_id`, so an app server that
  passed a browser-supplied `socket_id` through could be tricked into
  signing a token that also opened another `private-` or `presence-`
  channel. The MAC input is now unambiguous (length-prefixed fields), the
  token's `jti` must be 32 lowercase hex characters, and `/v1/auth/sign`
  rejects a `socket_id` that is not a ULID and a channel that is not a valid
  `private-`/`presence-` name. Tokens are still opaque strings in the same
  format; any issued by an older server stop verifying, which a restart
  already caused.
- Revoking an API key now closes that key's open WebSocket connections with
  close code 1008 and reason `key revoked`. Before, a revoked key's live
  sockets kept working until they disconnected on their own.
- One connection could exhaust its API key's shared rate budget with junk
  subscribe or unsubscribe frames and lock out every other client on the
  key. Per-connection limits now apply first (20 subscribe or unsubscribe
  frames a second with a burst of 64, and the existing 50 publishes a second
  with a burst of 100) and answer with `RATE_LIMITED_CONN`; a frame they
  reject spends none of the key's budget.
- Outbound frames were HTML-escaped, so a 60 KB publish of `<` reached every
  subscriber as a 360 KB event, a sixfold amplifier applied before the
  fanout copies it. Events, acks and errors are now encoded without HTML
  escaping, and an error frame no longer echoes a channel name too long to
  be valid.
- The per-IP connection cap counts an IPv6 client by its /64 prefix, so
  rotating addresses within one allocation no longer avoids it.
- Go toolchain 1.26.8, which fixes six standard-library vulnerabilities
  reachable from wirefan. Removing the OpenTelemetry hook (see Removed)
  drops gRPC and with it the last reachable advisory. CI now runs
  `govulncheck` on every change.

### Added

- `@wirefan/client` in `clients/js`: a zero-dependency TypeScript client
  for browsers and Node 22+ with reconnect (exponential backoff and jitter),
  automatic resubscription that re-authorizes private channels with fresh
  tokens, awaitable subscribe and unsubscribe, typed errors and a bounded
  connect handshake (`handshakeTimeoutMs`). The demo page is rebuilt on it.
  It is versioned separately (0.x) and not yet published to npm.
- Error frames carry optional `op` and `channel` fields naming the request
  they answer, so a client can match an error to the subscribe, unsubscribe
  or publish that caused it. Additive within protocol `v1`. `channel` is
  left out when the offending name is longer than 128 bytes.
- `wirefan --version`, stamped with the release tag at build time. The
  startup log line includes it too.
- Release binaries for linux/amd64 and linux/arm64, built natively on each
  architecture, smoke-tested, published with `SHA256SUMS`, and with the
  minimum glibc of each binary listed in the release notes.
- `deploy/provision.sh` takes a fresh Ubuntu 24.04 VPS to a running,
  TLS-terminated service, and `deploy/deploy.sh` upgrades it: checksum
  verification, a snapshot of the previous binary and key database, a health
  check, and automatic rollback of both if the check fails.
  `docs/DEPLOY.md` is a provider-agnostic runbook built around them, with
  appendices for Docker, Cloudflare Tunnel and the Cloudflare proxy.
- Versioned SQLite schema with a transactional, forward-only migration
  runner. A v0.2.0 database is adopted in place; a database from a newer
  wirefan, or one wirefan did not create, is refused without being
  modified.
- CI jobs for the JavaScript client (unit and end-to-end tests against a
  server built from the same commit, plus a check that the demo's vendored
  copy is current) and for `govulncheck`. A tag only releases after the full
  CI suite passes.
- `docs/COMPATIBILITY.md`, `SECURITY.md` with private vulnerability
  reporting, `CONTRIBUTING.md`, issue and pull-request templates, and
  Dependabot.
- README diagrams designed in Figma: architecture, message lifecycle,
  private-channel token flow, release pipeline, upgrade and rollback, and a
  benchmark chart.

### Changed

- Metric renames, before names are frozen: `wirefan_connections_total` is
  now `wirefan_connections`, and `wirefan_channels_total` is now
  `wirefan_channels`. Both are gauges, and the `_total` suffix is reserved
  for counters. Update dashboards that use the old names. Every collector
  now has help text.
- `wirefan_channels` reports the live channel count from the registry.
  It previously read 0 forever.
- Re-subscribing to a channel the connection already holds succeeds without
  a token, including on `private-` and `presence-` channels.
- `deploy/wirefan.service` binds the public listener to `127.0.0.1:8080`
  behind Caddy, and the shipped Caddyfile no longer overwrites
  `X-Forwarded-For`, so the Cloudflare trusted-proxy setup works as
  documented.
- The demo page uses the same dark palette and mark as the README visuals,
  and `docs/demo.gif` is re-recorded from a 1.0 build.
- `--help` exits 0.
- Release notes come from this changelog instead of an auto-generated list.

### Fixed

- A client that sent no data frames for 60 seconds was disconnected even
  while answering every ping, because pongs did not reset the read
  deadline. Healthy idle connections now stay open; a peer that does not
  answer a ping within 10 seconds is disconnected.
- When a pump exited on an error (an oversized message, for example), the
  server sent a close frame but left the TCP connection open. Every exit
  path now closes the socket.
- Shutdown closed connections one at a time and could take about five
  seconds per unresponsive peer, well past the 30 second drain window.
  Connections are now closed concurrently and force-closed when the window
  ends. A peer that stalls in the middle of a frame can no longer wedge a
  close handshake and keep its socket open.
- A subscribe that raced the channel sweeper could fail with
  `SUBSCRIBE_FAILED`; channels are now removed from the registry in the
  same step that marks them deleted.
- Refusing a foreign database in WAL mode could checkpoint its WAL into the
  main file. The pre-open inspection is now read-only and leaves a refused
  file untouched. The schema version and table list are read in one
  transaction, so a concurrent opener's migration cannot trigger a false
  refusal.
- `_wirefan-stats` events use ULID ids like every other event.
- `@wirefan/client`: a channel was dropped for good if its automatic
  resubscribe failed transiently (transient failures now retry with
  backoff; only definitive refusals drop the channel); errors could be
  attributed to the wrong pending request; a pending unsubscribe waited for
  a timeout instead of settling on its own error; a subscribe that raced a
  reconnect could leave an orphaned channel or bind its handle to the wrong
  record; the `connected` event reported `reconnected: true` on a first
  connection after a failed dial; a token minted for a dead socket could be
  sent after a reconnect; and a connect could hang forever on an upgrade
  that never produced a `connected` frame.
- `deploy/provision.sh` did not reload Caddy when run without `--binary`,
  and re-running it overwrote hand edits and did not restart the service.
- `make release-local` failed on Linux with rootful Docker, and now names
  its artifacts the way the release workflow does.
- `scripts/bench.sh` lost its failure diagnostics when the load generator
  failed, and profiling runs overwrote the committed profiles.
- Documentation was corrected in many places against the code, including
  the heartbeat and close-code contract, the token format, error-frame
  semantics, rate limits, backpressure policies, deployment topology and
  benchmark parameters.

### Removed

- The OpenTelemetry hook. It was only ever called with an empty endpoint
  and nothing could enable it, but it linked OpenTelemetry and gRPC into
  the binary.

## [0.2.0] - 2026-08-06

The headline of this release is evidence. v0.1.0 described a server; v0.2.0
measures one. Every performance figure now published traces to a raw output
file committed under `results/`, and every one of those runs reconciles the
load generator's sent count against the server's own
`wirefan_messages_published_total` counter.

### Added

- SQLite key persistence as the default store, selectable with `--store`
  and `--db-path`. API keys now survive a restart.
- `--registry` (`sync-map` | `sharded`) and `--fanout` (`per-conn` |
  `sharded`) implementation selection. The pluggable strategies were always
  present in the code but could not be chosen at runtime, which made the
  benchmark matrix meaningless and the swappability claim unverifiable.
- `WIREFAN_IP_CAP` environment override for the per-IP connection cap.
- Read-only client subscribe to the `_wirefan-stats` system channel, so the
  demo's live stats panel shows real values. Publishing to it is still
  rejected, and every other underscore-prefixed channel remains reserved.
- Published benchmark results across a `Fanout x Registry` matrix at 100,
  1,000 and 5,000 connections, plus the default configuration at 500, with
  43 raw output files, CPU and heap profiles, and a reproducible
  `docker run --cpus=1 --memory=6g` methodology.
- A recorded demo showing real two-tab fan-out with live stats.
- CI build badge and this changelog.

### Changed

- The default store is now SQLite rather than in-memory. Pass
  `--store=memory` for the previous behavior. Pre-1.0, so this default
  change lands in a minor release, but it is a behavior change worth noting.
- Benchmark methodology now spreads publishers across a pool of API keys.
  The previous single-key design meant the per-key rate limiter (100 msg/s,
  burst 200) was the thing being measured rather than the fan-out engine.
- Latency output is reported in microseconds instead of truncating to `0s`.

### Fixed

- The web demo never worked. `web/client.js` sent `event` where the server
  expects `type`, so subscribe and publish failed end to end.
- `deploy/Dockerfile` could not boot as the nonroot user because the state
  directory was not writable.
- The systemd unit and the benchmark script omitted required flags.
- The benchmark script accepted zero-throughput cells as results. It now
  exits nonzero when a cell produces no throughput or the server dies.
- The load generator silently swallowed dial and handshake failures. It now
  reports attempted, connected, dial_failed, sub_failed and died_early, for
  subscriber-only connections as well as publishers.
- `ShardedPool` had a Broadcast/Close race and no shutdown hook. `Fanout`
  now has `Close`, wired into the server's shutdown path, and the
  goroutine-leak invariant is proven under both fanout strategies.
- CI pinned Go 1.25 while `go.mod` targets 1.26, which would have failed on
  the first push.
- Documentation contradicted the code in several places: the ordering
  guarantee, the admin-token flow, the subscribe-token wire format, and the
  architecture diagram's own labels.

### Removed

- A fabricated performance claim on the social card that was never measured.
- The Centrifugo head-to-head benchmark section.
- Placeholder benchmark rows pending hardware that did not exist.
- The unused `ErrKeyRevoked` sentinel.

### Security

- Go toolchain and dependency updates. `govulncheck` reports no findings.

## [0.1.0] - 2026-05-07

Initial release. Single-binary Go WebSocket fan-out server with channel-based
pub/sub, HMAC-bound subscribe tokens with replay protection, per-subscriber
FIFO delivery, graceful drain, Prometheus metrics and zero runtime
dependencies.

[Unreleased]: https://github.com/EthanY33/wirefan/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/EthanY33/wirefan/compare/v0.2.0...v1.0.0
[0.2.0]: https://github.com/EthanY33/wirefan/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/EthanY33/wirefan/releases/tag/v0.1.0
