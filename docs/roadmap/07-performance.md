# 07. Performance

Epic label: `epic:performance`. Tracking issue: [#101](https://github.com/EthanY33/wirefan/issues/101). Milestones: v1.1 (measurement fixes), v1.3
(measurements and changes). Estimate: 50 h.

## Goal

Make the published numbers trustworthy first, then measure the cases the
design is most likely to fail at, and change code only where a measurement
shows a gain:

1. Microbenchmarks for the publish hot path, tracked per PR.
2. A load generator that does not drop latency samples or suffer from
   coordinated omission.
3. Results for hot channels and large fan-out, for 2 to 8 cores, and for
   memory per idle connection.
4. Zero-allocation broadcast and frame encoding, if the benchmarks justify
   it.
5. A decision, backed by numbers, on permessage-deflate.

## Current state

* **No microbenchmarks.** There is no `func Benchmark` in any `_test.go`.
  `docs/BENCHMARKS.md` (lines 26-37) says no number has been measured on a
  1.0 build. The only harness is the Docker matrix in `scripts/bench.sh`,
  which needs Docker and takes many minutes.
* **Load generator samples.** `cmd/loadtest/main.go`: latency samples go
  into a channel of size `conns*16` (line 108) through `select` with a
  `default` branch that silently drops them (lines 404-406); results are
  sorted with `sort.Slice` (around line 233); publishers use
  `time.NewTicker` (line 416) and measure from the actual send time, so a
  stalled publisher under-reports latency. `scripts/bench.sh` (around line
  194) scrapes the `SUMMARY` line with awk.
* **Uniform, small channels.** The load generator assigns channel
  `loadtest-(i % channels)` (around line 144); every published result uses
  10 subscribers per channel (`docs/BENCHMARKS.md` line 66). The
  per-channel cap is 10 000 (`docs/PROTOCOL.md` 9).
* **Hot channel behavior.** `internal/fanout/sharded.go: shardFor` (lines
  47-51) hashes the channel name with `fnv.New32a` over `[]byte(name)`, so
  one hot channel maps to one worker. `ShardedPool.Broadcast` blocks on a
  1024-slot shard queue while holding `mu.RLock` (around line 63). No metric
  exposes queue depth.
* **Per-publish allocations.** `internal/hub/channel.go: Broadcast` (lines
  52-62) builds a new `[]registry.Subscriber` per publish (about 160 KB at
  the 10 000 cap). `handlePublish` builds a `map[string]any` (around line
  235) encoded by `marshalFrame` (around line 288).
* **One CPU.** `docs/BENCHMARKS.md` (lines 11-24, 47-56): every run uses a
  container with one CPU of quota (GOMAXPROCS 2) and a load generator on the
  same host. `bench.sh` accepts `CPUS` but no result exists above 1.
  `cmd/wirefan/main.go:140` sizes the sharded pool by `GOMAXPROCS`.
* **Memory per connection.** `conn.Run` starts two pump goroutines
  (`internal/conn/conn.go` lines 221-222), a 256-slot send channel, two
  `rate.Limiter`s and a close-request channel. `docs/BENCHMARKS.md` (lines
  210-211) gives only "about 10 MB in use" at 500 connections with bufio
  buffers about 40 percent of heap. The demo runs on a 1 GB e2-micro.
* **Compression.** `internal/server/upgrade.go:294` builds
  `AcceptOptions` with only `OriginPatterns`; `docs/PROTOCOL.md` 2 says
  compression is not negotiated. `coder/websocket` v1.8.15 supports
  `CompressionMode`.

## Proposed design

### Measurement first (v1.1)

**Microbenchmarks**, all with `b.ReportAllocs`:

| Benchmark | Package | Parameters |
| --------- | ------- | ---------- |
| `BenchmarkBroadcast` | `internal/hub` | 10, 100, 1 000, 10 000 subscribers |
| `BenchmarkMarshalFrame`, `BenchmarkHandlePublish` | `internal/conn` | fake ws, `PolicyDropNewest` |
| `BenchmarkConnSend` | `internal/conn` | per policy |
| `BenchmarkShardedPoolBroadcast`, `BenchmarkShardFor` | `internal/fanout` | |
| `BenchmarkRegistryGetOrCreate` | `internal/registry` | parallel, both registries |

`.github/workflows/bench.yml` runs on PRs touching `internal/**`: `go test
-run=^$ -bench=. -count=10` on base and head, `benchstat` comparison in the
job summary, a warning (not a failure) above a 10 percent regression because
shared runners are noisy. `docs/BENCHMARKS.md` gains a Microbenchmarks
section explaining how it relates to the Docker matrix.

**Load generator histogram.** Replace the samples channel and slice with
per-goroutine log-linear histograms (hand-rolled buckets, no new
dependency) merged at the end. Latency is measured from each publish's
intended send time on the schedule, which removes coordinated omission.
`SUMMARY` gains `samples=`, `lost=0`, `p90`, `p999` and `max`, and a
`--json` flag gives `bench.sh` a machine-readable result instead of awk.

### Measurements (v1.3)

**Queue depth metric.** `wirefan_fanout_queue_depth{shard}` (gauge read at
scrape time) and `wirefan_fanout_enqueue_wait_seconds` (histogram) on
`ShardedPool`. Shard count equals `GOMAXPROCS`, so the label is bounded.
This lands before the hot-channel runs so their results can explain where
time goes.

**Hot channel and large fan-out.** Load generator flags
`--subs-per-channel`, `--zipf` (Zipf-distributed channel popularity) and a
single-channel mode with one publisher and up to 10 000 subscribers. Runs at
100, 1 000, 5 000 and 10 000 subscribers on one channel and with a Zipf
skew, for both fanouts, reporting per-subscriber lag, drops and slow-consumer
disconnects. Findings that point at a design change (for example splitting a
large subscriber set across workers) become follow-up issues with the data
attached.

**Multi-core scaling.** The matrix at `CPUS=1,2,4,8` on a Linux host with
`--cpuset-cpus` (so GOMAXPROCS equals the core count) and the load generator
on a separate machine. `bench.sh` records host CPU model, core count and
kernel in each results header and refuses to run the load generator on the
same host unless `LOADGEN_LOCAL=1`. The fanout recommendation in
`docs/DESIGN.md` and README is confirmed or changed with the data cited.

**Idle memory curve.** Load generator `--hold` mode (dial, subscribe once,
idle) and `scripts/memcurve.sh` for 1k, 2k, 5k, 10k and 20k connections in a
memory-capped container, recording `process_resident_memory_bytes`,
`go_goroutines`, `go_memstats_heap_inuse_bytes` and
`go_memstats_stack_inuse_bytes`. `docs/BENCHMARKS.md` states bytes and
goroutines per idle connection and the measured number that fits in 1 GB.
A CI guard test opens 2 000 in-process connections and fails if bytes per
connection exceed the recorded budget by more than 20 percent.

### Changes gated on measurements (v1.3)

**Allocation removal.**

1. `registry.Channel` keeps a copy-on-write subscriber slice behind an
   `atomic.Pointer`, rebuilt on `Subscribe` and `Unsubscribe`, so
   `Broadcast` iterates a stable slice with no allocation and no lock. The
   `Deleted` / `ErrChannelDeleted` protocol with the sweeper is preserved.
2. The publish frame becomes a typed struct with fields in the order
   `channel`, `data`, `id`, `type` (the order `encoding/json` gives the
   current map), encoded into a pooled buffer, so output stays
   byte-identical.
3. `shardFor` computes FNV-1a inline over the string with no conversion.

Each change ships only if `benchstat` shows fewer allocs/op and the Docker
matrix shows no throughput loss.

**Compression spike.** `--compression=off|no-context-takeover|context-takeover`
mapped to `AcceptOptions.CompressionMode`, with `CompressionThreshold` (for
example 512 bytes) so small frames skip compression. Load generator options
for payload size and compressibility; runs at 100 and 1 000 connections with
200 B, 2 KB and 20 KB JSON payloads per mode on the demo VM class, recording
CPU, p99 latency, memory per connection (context takeover keeps a window per
connection) and bytes on the wire. The flag ships default `off`, and
`docs/DEPLOY.md` recommends a mode only if it wins.

## Protocol and API changes

| Change | Kind |
| ------ | ---- |
| `wirefan_fanout_queue_depth`, `wirefan_fanout_enqueue_wait_seconds` | New metrics |
| `--compression` | New flag, default off |
| permessage-deflate negotiated when enabled | `docs/PROTOCOL.md` 2 updated; clients that do not offer it are unaffected |
| `cmd/loadtest` flags `--json`, `--subs-per-channel`, `--zipf`, `--hold`, payload options | Tooling |

## Compatibility impact

Additive. With `--compression=off` (the default), the handshake is
unchanged. The frame encoding change is guarded by a golden test so event
bytes do not change. Performance characteristics are not covered by
`docs/COMPATIBILITY.md`, but every changed number is recorded with its raw
files under `results/`.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#52](https://github.com/EthanY33/wirefan/issues/52) | v1.1 | 6 h |
| 2 | [#53](https://github.com/EthanY33/wirefan/issues/53) | v1.1 | 5 h |
| 3 | [#88](https://github.com/EthanY33/wirefan/issues/88) | v1.3 | 3 h |
| 4 | [#89](https://github.com/EthanY33/wirefan/issues/89) | v1.3 | 6 h |
| 5 | [#90](https://github.com/EthanY33/wirefan/issues/90) | v1.3 | 6 h |
| 6 | [#91](https://github.com/EthanY33/wirefan/issues/91) | v1.3 | 8 h |
| 7 | [#92](https://github.com/EthanY33/wirefan/issues/92) | v1.3 | 8 h |
| 8 | [#93](https://github.com/EthanY33/wirefan/issues/93) | v1.3 | 8 h |

Wall-clock time for benchmark runs is not included. Multi-core runs need a
second machine, which has a cloud cost.

## Test plan

* Microbenchmarks run in under three minutes with `go test -bench=.
  -benchmem ./internal/...`; the bench workflow posts a benchstat table on a
  PR touching `internal/hub`.
* Histogram quantiles match an exact sort on a one-million-sample test
  within bucket error; load generator memory is constant in run duration.
* Hot channel, multi-core, memory and compression results each commit raw
  files under `results/` and a section in `docs/BENCHMARKS.md`.
* Allocation removal: `BenchmarkBroadcast` at 10 000 subscribers reports
  O(1) allocations per publish; a golden test proves byte-identical frames;
  the Subscribe/Sweep race tests pass under `-race`.
* Compression: a handler test proves a deflate-offering client negotiates
  the extension only when the flag enables it.

## Open questions

* Should a full shard queue fail the publish with a new error code instead
  of blocking the publisher's read loop? Decide from the queue-depth data.
* If one worker per hot channel is the bottleneck, is chunked fan-out of a
  large subscriber set across workers worth losing the sharded fanout's
  per-channel serialization? That is a design question for a follow-up.
* Which cloud instance types for the multi-core runs, given that the demo
  runs on an e2-micro? Proposed: one 2-vCPU and one 8-vCPU type, with the
  load generator on a third.
