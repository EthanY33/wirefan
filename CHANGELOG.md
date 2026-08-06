# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
  500, 1,000 and 5,000 connections, with 45 raw output files, CPU and heap
  profiles, and a reproducible `docker run --cpus=1 --memory=6g` methodology.
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

- The web demo never worked. `web/client.js` sent `type` where the server
  expects `event`, so subscribe and publish failed end to end.
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

## [0.1.0] - 2026-05-04

Initial release. Single-binary Go WebSocket fan-out server with channel-based
pub/sub, HMAC-bound subscribe tokens with replay protection, per-subscriber
FIFO delivery, graceful drain, Prometheus metrics and zero runtime
dependencies.

[0.2.0]: https://github.com/EthanY33/wirefan/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/EthanY33/wirefan/releases/tag/v0.1.0
