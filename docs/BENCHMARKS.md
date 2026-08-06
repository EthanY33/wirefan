# wirefan benchmarks

Every number in this document comes from a committed raw file under
[`results/`](../results/). Each results row links the exact file it was read
from. Nothing here is estimated, extrapolated, or rounded up. If a
configuration is not in the tables, it did not produce a clean run and was
not published.

## Environment

| Component | Value |
|---|---|
| Server container | `docker run --cpus=1 --memory=6g` (Docker 29.6.1, Docker Desktop / WSL2) |
| Server binary | linux/amd64, Go 1.26.5, built via `deploy/Dockerfile` (`golang:1.26-bookworm` builder, distroless runtime) |
| Host CPU | Intel Core i5-11600KF (6C/12T, 3.9 GHz base), 16 GB RAM, Windows 11 |
| Load generator | `cmd/loadtest`, Go 1.26.5 windows/amd64, running on the host against the container's published ports |
| Store | `--store=memory` (hermetic; fresh state per cell) |
| Per-IP cap | `WIREFAN_IP_CAP=20000` (all load-generator conns share one source IP) |

The server is pinned to 1 CPU to model the smallest practical deployment
target. The load generator runs on the same machine, so traffic crosses
loopback plus the Docker Desktop port proxy. There is no real network RTT
in any latency figure below; treat latencies as server-processing plus
local-stack time, not as end-to-end WAN numbers.

## Methodology

The matrix exercises the two flag-selectable axes:

| Axis | Variants |
|---|---|
| `--fanout` | `per-conn` (default), `sharded` (worker pool sized to GOMAXPROCS) |
| `--registry` | `sync-map` (default), `sharded` (16 shards, RWMutex+map) |

Each cell:

1. Boots a fresh container with the cell's `--fanout`/`--registry` flags.
2. Mints a pool of API keys against that cell's own admin endpoint. The
   server rate-limits publishes at 100 msg/s (burst 200) per API key, so the
   pool is sized to keep every key far below that: 1 key per 10 connections
   at 100 to 1,000 conns (10, 50, 100 keys), 1 key per 20 connections at
   5,000 conns (250 keys). Connections round-robin across the pool.
3. Opens N WebSocket clients over a ramp-up, distributed across K channels
   (10 subscribers per channel at every scale). Half of the connections
   publish at a fixed per-connection rate for 30 seconds.
4. Repeats 3 times. The published row is the median-throughput repetition
   and every figure in the row comes from that single repetition's raw file.

A run only counts as clean if every connection dialed, subscribed, and
survived the full duration, and the server sent zero error frames
(`cmd/loadtest` exits nonzero otherwise, which aborts the whole matrix).
Cells that aborted on a transient dial or subscribe failure were rerun in
full; only fully clean 3-repetition cells appear here. Per-connection
publish rates were chosen per scale so the aggregate offered load stays
within what the 1-vCPU container sustains cleanly; probe runs above these
rates failed that bar and are not published.

Two independent honesty checks are recorded in every raw file:

- **Cross-check**: the client-side sent count is compared against the
  server's own `wirefan_messages_published_total` counter delta. All 39
  published repetitions match exactly.
- **Server-side latency**: the `wirefan_broadcast_latency_seconds`
  histogram is scraped after each run. The container's Linux clock resolves
  nanoseconds; the Windows host wall clock quantizes client-observed
  latency at roughly 0.5 ms (the measured tick is printed in each raw file
  as `host clock res`), so client percentiles are floor-limited at that
  granularity.

"Delivered msg/s" is messages received by subscribers per second
(fan-out output, not publish input). "Broadcast mean" is the mean time a
publish spends in the server's broadcast call: for `per-conn` fanout that
covers enqueueing to every subscriber's send buffer; for `sharded` fanout
it covers handoff to the worker pool, which is why it reads lower.

## Results

Scales were stepped 100 to 1,000 to 5,000 connections; all three completed
cleanly (5,000 was the largest scale attempted). 50% of connections
publish. Median repetition of 3; each row links its raw file, which
includes the exact docker invocation.

### 100 connections, 10 channels, 10 msg/s per publisher

| Fanout | Registry | Sent | Delivered | Delivered msg/s | Client p50 | Client p99 | Broadcast mean | Raw |
|---|---|---|---|---|---|---|---|---|
| per-conn | sync-map | 14,983 | 141,797 | 4,727 | 1.09 ms | 3.20 ms | 16.9 us | [raw](../results/per-conn-sync-map-c100-rep3.txt) |
| per-conn | sharded | 14,989 | 141,815 | 4,727 | 1.10 ms | 3.52 ms | 17.4 us | [raw](../results/per-conn-sharded-c100-rep3.txt) |
| sharded | sync-map | 14,979 | 141,764 | 4,725 | 1.15 ms | 3.83 ms | 7.0 us | [raw](../results/sharded-sync-map-c100-rep2.txt) |
| sharded | sharded | 14,990 | 141,844 | 4,728 | 1.08 ms | 2.71 ms | 10.8 us | [raw](../results/sharded-sharded-c100-rep3.txt) |

### 1,000 connections, 100 channels, 3 msg/s per publisher

| Fanout | Registry | Sent | Delivered | Delivered msg/s | Client p50 | Client p99 | Broadcast mean | Raw |
|---|---|---|---|---|---|---|---|---|
| per-conn | sync-map | 44,990 | 422,570 | 14,086 | 1.06 ms | 2.57 ms | 23.1 us | [raw](../results/per-conn-sync-map-c1000-rep3.txt) |
| per-conn | sharded | 44,991 | 422,582 | 14,086 | 1.05 ms | 2.82 ms | 24.5 us | [raw](../results/per-conn-sharded-c1000-rep2.txt) |
| sharded | sync-map | 44,989 | 422,568 | 14,086 | 1.06 ms | 2.75 ms | 12.5 us | [raw](../results/sharded-sync-map-c1000-rep2.txt) |
| sharded | sharded | 44,997 | 422,588 | 14,086 | 1.06 ms | 3.25 ms | 11.6 us | [raw](../results/sharded-sharded-c1000-rep3.txt) |

### 5,000 connections, 500 channels, 0.5 msg/s per publisher

| Fanout | Registry | Sent | Delivered | Delivered msg/s | Client p50 | Client p99 | Broadcast mean | Raw |
|---|---|---|---|---|---|---|---|---|
| per-conn | sync-map | 37,480 | 278,671 | 9,289 | 0.56 ms | 23.4 ms | 25.4 us | [raw](../results/per-conn-sync-map-c5000-rep2.txt) |
| per-conn | sharded | 37,484 | 278,691 | 9,290 | 0.56 ms | 15.9 ms | 24.8 us | [raw](../results/per-conn-sharded-c5000-rep3.txt) |
| sharded | sync-map | 37,481 | 278,678 | 9,289 | 0.59 ms | 25.8 ms | 14.1 us | [raw](../results/sharded-sync-map-c5000-rep3.txt) |
| sharded | sharded | 37,485 | 278,685 | 9,290 | 0.57 ms | 8.0 ms | 13.8 us | [raw](../results/sharded-sharded-c5000-rep2.txt) |

### Peak sustained delivery (500 connections, 50 channels, 10 msg/s per publisher, defaults: per-conn/sync-map)

| Sent | Delivered | Delivered msg/s | Client p50 | Client p99 | Broadcast mean | Raw |
|---|---|---|---|---|---|---|
| 74,982 | 707,039 | 23,568 | 1.06 ms | 6.68 ms | 20.5 us | [raw](../results/per-conn-sync-map-c500-rep3.txt) |

23,568 delivered msg/s is the highest clean sustained rate measured on 1
vCPU with this harness. It is load-bound at 10 subscribers per channel;
different channel shapes will produce different ceilings.

## Reading the matrix

- At these offered loads every cell delivers the full load (delivered
  throughput is identical across cells at each scale), so the axes do not
  differentiate on throughput here. They differentiate on where time is
  spent.
- `sharded` fanout roughly halves the time the publisher spends in the
  broadcast call (7 to 14 us vs 15 to 25 us) because it hands the write
  work to a worker pool instead of enqueueing every subscriber inline. On
  a 1-CPU container that does not translate into more delivered
  throughput; the same core still does the socket writes.
- The registry axis is not visible at this channel-churn rate: channels are
  created once and then only read. A subscribe/unsubscribe-heavy workload
  would be needed to separate `sync-map` from `sharded`.

## CPU and heap profile (headline cell)

Captured with `PROFILE_CELL=per-conn-sync-map` during a separate,
non-published run of the peak cell (profiling adds overhead, so the
profiled repetition's numbers are not in the tables). Rendered with
`go tool pprof -top`; text output committed:

- [`docs/profiles/per-conn-sync-map-c500-cpu-top.txt`](profiles/per-conn-sync-map-c500-cpu-top.txt):
  45.9% of samples are in `Syscall6` under `websocket.(*Conn).writeFrame`
  (53.5% cumulative). The hot path is socket writes, not wirefan
  bookkeeping; the registry and hub do not appear in the top nodes.
- [`docs/profiles/per-conn-sync-map-c500-heap-top.txt`](profiles/per-conn-sync-map-c500-heap-top.txt):
  about 10 MB in use under load; the top consumers are the per-connection
  bufio read/write buffers (about 40% combined).

Raw protobuf profiles: [`results/per-conn-sync-map-c500-cpu.pb.gz`](../results/per-conn-sync-map-c500-cpu.pb.gz),
[`results/per-conn-sync-map-c500-heap.pb.gz`](../results/per-conn-sync-map-c500-heap.pb.gz).

## Reproducing

```
go build -o bin/loadtest ./cmd/loadtest        # bin/loadtest.exe on Windows
docker build -f deploy/Dockerfile -t wirefan:bench .
bash scripts/bench.sh                          # full matrix at CONNS=1000
```

Per-scale invocations used for the tables above:

```
CONNS=100  CHANNELS=10  RATE=10  REPS=3 DURATION=30s bash scripts/bench.sh
CONNS=1000 CHANNELS=100 RATE=3   REPS=3 DURATION=30s bash scripts/bench.sh
CONNS=5000 CHANNELS=500 RATE=0.5 RAMPUP=20s KEYS_PER_CONNS=20 REPS=3 DURATION=30s bash scripts/bench.sh
CONNS=500  CHANNELS=50  RATE=10  REPS=3 DURATION=30s CELLS="per-conn/sync-map" bash scripts/bench.sh
```

`scripts/bench.sh` fails hard on any server death, mint failure, dial or
subscribe failure, mid-run disconnect, server error frame, or
zero-throughput cell. The exact docker invocation, key-pool size, and both
honesty checks are recorded in every `results/*.txt` file.

An ARM row (Ampere A1, the intended production target) may be added later;
no ARM numbers exist yet.
