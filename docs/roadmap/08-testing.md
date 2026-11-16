# 08. Testing

Epic label: `epic:testing`. Tracking issue: [#102](https://github.com/EthanY33/wirefan/issues/102). Milestones: v1.1, v1.2, v1.3. Estimate: 45 h.

## Goal

Find the bugs the hand-picked test suite cannot: inputs nobody thought of,
platform differences, timing flakes, slow leaks, and regressions in the demo
page. Make coverage and lint findings visible in CI.

## Current state

* **No fuzzing.** No `func Fuzz` exists in any `_test.go`. The
  attacker-facing parser is `Conn.handle` in `internal/conn/handler.go`
  (lines 104-130): `utf8.Valid`, `json.Unmarshal` into `incoming` (with a
  `json.RawMessage` data field), `ValidateChannelName` (lines 26-41), then
  dispatch. `marshalFrame` (lines 288-294) re-embeds client JSON in frames
  relayed to every subscriber. `auth.VerifyTokenAgainst`
  (`internal/auth/token.go`, lines 92-120) parses attacker strings with
  `SplitN`, `ParseInt`, base64 and a jti check; `macPayload` (lines 67-76)
  carries a length-prefix fix for a delimiter-confusion forgery
  (`S|private-victim`). `token_test.go` (164 lines) and `handler_test.go`
  (698 lines) test named cases only.
* **Sharded shutdown is untested end to end.** All tests in
  `internal/server/shutdown_test.go` use `fanout.NewPerConn()` (lines 40,
  126, 188, 290); only `leak_test.go` uses `NewShardedPool(4)`.
  `Server.Shutdown` (`internal/server/server.go`, lines 175-189) documents
  that the pool must close after `Drain`, which no test checks.
* **Lint.** No `.golangci.yml`; CI runs golangci-lint v2.11.4 with the
  default linter set, and `make lint` is a bare `golangci-lint run`.
* **Coverage and flakes.** No coverage step in CI (`docs/DESIGN.md` 14 "Not
  in 1.0" notes the spec's 70 percent gate is not enforced). The suite has
  25 `time.Sleep` calls and no `t.Parallel`. The 65 s
  `TestKeepaliveIdleReaderOutlivesOldCutoff`
  (`internal/conn/pumps_test.go`, lines 107-115) runs in every CI run
  because CI does not pass `-short`. `shutdown_test.go` (lines 72-74) uses a
  100 ms settle sleep. Per-package coverage has not been measured.
* **Platforms.** `ci.yml` runs build and `go test -race` on `ubuntu-latest`
  only; `release.yml` (line 42) runs an arm64 race pass only at tag time.
  `CONTRIBUTING.md` (line 20) documents Windows builds with MinGW.
* **Load generator is untested.** `cmd/loadtest` has no tests; CI only
  compiles it. Its exit codes 2 to 7 (`main.go` lines 261-276) and the
  cross-checks in `scripts/bench.sh` never run in CI. There is no
  shellcheck step.
* **Demo page is untested.** `web/client.js` (1,516 lines: publish budget at
  lines 37-38, `diagnoseFirstFailure` at 531-551, `BC_NAME` tab coordination
  at 53), `web/diagram.js` and `web/index.html` have no automated tests; CI
  only diffs the vendored client file.
* **No soak.** Leak coverage is the 1,000-connection churn test in
  `internal/server/leak_test.go`. Nothing runs for hours or watches heap,
  file descriptors, `Hub.closedKeys` (never pruned, `hub.go` lines 27-30),
  ratelimit entries, `auth.ReplayCache` or the registry over time.

## Proposed design

### Fuzzing (v1.1)

`internal/conn/handler_fuzz_test.go`:

* `FuzzHandle`: a `Conn` built from the `handler_test.go` fixtures (memory
  registry, `PolicyDropNewest`, buffered send channel). Arbitrary bytes into
  `c.handle`; assert no panic and that every frame drained from `c.send` is
  `utf8.Valid` and `json.Valid`.
* `FuzzValidateChannelName`: accepted names are 1 to 128 bytes with no rune
  below 0x20 and no 0x7f, and acceptance equals that predicate.
* `FuzzMarshalFrame`: for any `json.Valid` data, the output unmarshals and
  its `data` equals the compacted input.

`internal/auth/token_fuzz_test.go`:

* `FuzzVerifyToken`: arbitrary token, socket id and channel return nil or an
  error from the defined set, never a panic.
* Property test: `SignToken` then `VerifyToken` succeeds for the same
  arguments and fails when the socket id, the channel or any single byte of
  the token changes (10 000 random tokens).
* `FuzzMacPayloadInjective`: distinct tuples produce distinct MAC inputs,
  including fields containing `|` and `:`.
* `ReplayCache`: 64 goroutines record the same jti under `-race`; exactly
  one succeeds.

Seed corpora under `testdata/fuzz` include the existing named cases and the
`S|private-victim` case. A scheduled workflow fuzzes each target for 30 s.

### Shutdown race tests (v1.1)

Shutdown tests parameterized over both fanouts: continuous publishes from N
clients while `Server.Shutdown` runs; assert every counted, delivered event
reached each still-connected subscriber or that subscriber was closed with
1001, no goroutines remain (reusing `settledGoroutines`), and no
send-on-closed-channel under `-race`. `hub_stress_test.go`: 200 goroutines
doing `Add`/`Remove` while `Drain` and `CloseKey` run; `Len()` is 0
afterwards and `Add` after drain always returns the drain close. The test is
verified to fail when `fan.Close` is moved before `Drain`.

### Lint config (v1.1)

`.golangci.yml` (v2 format) enabling `gosec`, `errorlint`, `bodyclose`,
`noctx`, `contextcheck`, `copyloopvar`, `prealloc`, `unparam`, `misspell`
and `nolintlint`, with a written reason for each exclusion. Real findings
are fixed in small, separate commits; intentional ones get `//nolint:<linter>
// reason`.

### Coverage and flake detection (v1.1)

1. Measure: `go test -race -coverprofile=cover.out ./...`, per-package
   numbers recorded in the PR.
2. Enforce: a CI step prints `go tool cover -func` and a script checks
   per-package floors set just below the measured values for `internal/conn`,
   `internal/server`, `internal/hub`, `internal/fanout`, `internal/auth` and
   `internal/store`. Floors only move up.
3. Nightly: `go test -race -shuffle=on -count=20 -short ./...`, opening an
   issue on failure. The 65 s keepalive test moves to the nightly job.
4. Fix flakes the nightly run finds with a polling helper
   (`eventually(t, cond, timeout)`), not longer sleeps.

### Load generator in CI (v1.1)

`scripts/smoke-loadtest.sh`, shared with `bench.sh`: boot `wirefan` with
`WIREFAN_ADMIN_TOKEN` in the environment, `--store=memory`, and `--listen`
and `--admin-addr` on free ports; mint keys with `POST /v1/keys`; run
`loadtest --conns=50 --dur=5s --rampup=1s`; assert exit 0 and the `SUMMARY`
fields (`dial_failed=0`, `sent>0`, `recv>0`). A negative case with a bad key
asserts exit code 3. `shellcheck` runs on `scripts/*.sh` and
`deploy/**/*.sh`.

### Platform matrix (v1.2)

The build job becomes a matrix over `ubuntu-latest`, `ubuntu-24.04-arm`,
`macos-latest` and `windows-latest` (cgo through the preinstalled MinGW on
Windows; `-race` where supported; `-short` on macOS and Windows if needed).
Lint, vuln and client jobs stay on Ubuntu. OS-specific failures are fixed or
skipped with a `t.Skip` that names the reason. `CONTRIBUTING.md` lists the
tested platforms.

### Demo browser test (v1.2)

Playwright project under `web/test`, Chromium only, with cached browsers.
It boots the real binary with `--dev` on a random port and a fresh key, then
asserts: the page connects; a pulse in one browser context arrives in a
second; more than four pulses in one second are throttled client-side; a
revoked key shows the bad-key overlay; killing the server shows the
reconnecting state and recovery after restart. Traces upload on failure. The
job starts as non-required and becomes required once the promotion
criterion in the open questions is met.

### Soak (v1.3)

Load generator `--churn`: connections dial, subscribe to random channels
from a large name space (including `private-` channels with signed tokens),
publish, and disconnect cleanly or abruptly, with periodic key creation and
revocation through the admin API. `scripts/soak.sh` boots the server,
samples `/metrics` every 30 s (`go_goroutines`,
`go_memstats_heap_inuse_bytes`, `process_open_fds`, `wirefan_connections`,
`wirefan_channels`) to CSV, and asserts a bounded slope after a 5 minute
warm-up, with the threshold stated in bytes per minute. A nightly workflow
runs 30 minutes and uploads the CSV; one 4 hour result is recorded in
`docs/BENCHMARKS.md`.

## Protocol and API changes

None. All changes are tests, CI and tooling.

## Compatibility impact

None on the binary. CI wall time for PRs must not grow after the 65 s test
moves to the nightly job; the platform matrix runs in parallel.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#57](https://github.com/EthanY33/wirefan/issues/57) | v1.1 | 4 h |
| 2 | [#54](https://github.com/EthanY33/wirefan/issues/54) | v1.1 | 4 h |
| 3 | [#55](https://github.com/EthanY33/wirefan/issues/55) | v1.1 | 3 h |
| 4 | [#56](https://github.com/EthanY33/wirefan/issues/56) | v1.1 | 4 h |
| 5 | [#58](https://github.com/EthanY33/wirefan/issues/58) | v1.1 | 5 h |
| 6 | [#59](https://github.com/EthanY33/wirefan/issues/59) | v1.1 | 3 h |
| 7 | [#76](https://github.com/EthanY33/wirefan/issues/76) | v1.2 | 6 h |
| 8 | [#70](https://github.com/EthanY33/wirefan/issues/70) | v1.2 | 8 h |
| 9 | [#94](https://github.com/EthanY33/wirefan/issues/94) | v1.3 | 8 h |

## Test plan

Each item is itself a test; its acceptance criteria are in its issue. The
epic is done when CI shows, on every PR: lint with the new config, coverage
per package against floors, the load generator smoke test, and the platform
matrix; and nightly: fuzzing, the shuffle/count run and the soak.

## Open questions

* The coverage floors are set from a measurement that has not been taken
  yet (a full `go test -cover ./...` with the cgo SQLite build is slow on
  Windows). The first PR of the coverage issue records the numbers.
* Should fuzzing run on every PR for a few seconds per target, or only on
  the schedule? Proposed: the seed corpus runs on every PR as ordinary tests;
  generated fuzzing runs only on the schedule.
* Promote the browser test to required after two weeks green, or after a
  fixed number of runs? Proposed: 50 consecutive green runs.
