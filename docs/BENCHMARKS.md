# wirefan benchmarks

> **Status:** methodology + tooling complete. Real numbers are captured on the
> Oracle Cloud Always Free ARM target (Task 32). This document is updated
> in-place once those runs land.

## Hardware target

Production reference run is performed on:

- **Oracle Cloud Always Free**, Ampere A1 (ARM64), 1 OCPU / 6 GB RAM
- Ubuntu 22.04 LTS, Linux 6.x kernel
- Single instance, Caddy reverse proxy on `:443` → wirefan on `:8080`
- No load shedding, no rate limit lifted from defaults (100 msg/s, burst 200)

Local-development numbers (Windows / Mac / Docker desktop) are recorded in
`results/local-*.txt` for reference but should NOT be cited as portfolio
numbers; the ARM single-vCPU target is the contract.

## Methodology

The matrix exercises the two pluggable axes the spec calls out:

| Axis | Variants |
|---|---|
| `Fanout` | `per-conn` (default), `sharded` (NumCPU×2 worker pool) |
| `Registry` | `sync-map` (default), `sharded` (16 shards, RWMutex+map) |

Each cell:

1. Boots wirefan with the chosen flags
2. `./bin/loadtest` opens **N=1000** WebSocket clients over a 5-second ramp-up
3. Clients are distributed across **K=100** distinct channels
4. **50%** of clients publish at **R=10 msg/s** (subscribers receive)
5. Run holds for **D=30s**
6. Captured: sent / received / drop ratio, latency p50 / p99 / p999

Latency is measured publisher-write → first-subscriber-receive on the same
host (kernel-level loopback). Real-world WAN latencies will be higher; the
benchmark numbers describe server-side processing latency only.

`make bench` reproduces the run end-to-end. `WIREFAN_KEY` env must be set to a
valid API key id (mint via `POST /v1/keys`).

## Stretch matrix (run when scaling test is needed)

| Conns | Channels | Rate | Notes |
|---|---|---|---|
| 1k | 100 | 10/s | Default; smoke shape |
| 10k | 100 | 10/s | Connection density |
| 25k | 1000 | 10/s | Channel-table contention |
| 50k | 1000 | 100/s | Headline number |

50k × 1000 × 100/s yields a sustained 5M msg/s through the broadcast path.

## Results — primary matrix (1k × 100 × 10/s × 30s)

> Numbers TBD — captured post-deploy on Oracle ARM. This table will be
> overwritten in-place with the real run output.

| Fanout | Registry | Sent | Recv | Recv/Sent | p50 | p99 | p999 | Max |
|---|---|---|---|---|---|---|---|---|
| per-conn | sync-map | TBD | TBD | TBD | TBD | TBD | TBD | TBD |
| per-conn | sharded | TBD | TBD | TBD | TBD | TBD | TBD | TBD |
| sharded | sync-map | TBD | TBD | TBD | TBD | TBD | TBD | TBD |
| sharded | sharded | TBD | TBD | TBD | TBD | TBD | TBD | TBD |

Raw outputs land in `results/<fanout>-<registry>.txt`.

## Profiles

CPU and heap flamegraphs are captured under load via `/debug/pprof/profile`
and `/debug/pprof/heap`, rendered to PNG with `go tool pprof`.

- `docs/profiles/per-conn-sync-map-cpu.png` — TBD
- `docs/profiles/per-conn-sync-map-heap.png` — TBD
- (one pair per matrix cell)

## Winner & hot-path explanation

> TBD post-run. Expected qualitative reasoning, to be confirmed:
>
> - **per-conn fanout** wins on small subscriber sets (≤ 100 per channel) — it
>   skips queue overhead and writes directly to per-conn buffered chans.
> - **sharded fanout** wins as channel cardinality and per-channel subscriber
>   counts grow — the worker pool keeps a single hot publisher from monopolizing
>   the GMP `M`s assigned to that publisher's goroutine.
> - **sync-map registry** wins on read-heavy mostly-stable channel sets — its
>   internal append-only `read` map is lock-free.
> - **sharded registry** wins on write-heavy churn (constant subscribe/unsub
>   patterns across many channels) — RWMutex on a small shard outperforms the
>   sync.Map's load-and-promote dance.

## Stretch: Centrifugo head-to-head

If wirefan ships ahead of schedule, run the same matrix against Centrifugo
(default config) and tabulate side-by-side. Marked stretch in the
implementation plan; not blocking.

## Reproducing locally

1. `go install` toolchain ≥ 1.25
2. `make build && make loadtest`
3. `./bin/wirefan &` — note the admin Bearer printed at startup
4. `curl -X POST -H "Authorization: Bearer <admin>" http://localhost:8080/v1/keys -d '{"name":"bench"}'` → save the returned `id`
5. `WIREFAN_KEY=<id> make bench`

The full matrix takes ~8 minutes locally (4 cells × 30s + dial overhead).

## Reproduction file inventory

```
scripts/bench.sh           # the runner this doc points at
cmd/loadtest/main.go       # the load generator
docs/BENCHMARKS.md         # this file
docs/profiles/             # PNGs land here
results/                   # raw .txt per cell (gitignored)
```

`results/` is gitignored — rerun bench to repopulate.
