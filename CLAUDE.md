# wirefan

Single-binary Go WebSocket fanout server: channel pub/sub, HMAC-signed subscribe tokens. Targets ~$0 hosting on Oracle Always Free. Repo: https://github.com/EthanY33/wirefan. Demo: TBD post-deploy.

## Build / test
CGO is required for all builds/tests, not just `-race` (`internal/store/sqlite.go` uses `mattn/go-sqlite3`). Go (`C:\Program Files\Go\bin`) and mingw gcc (WinLibs) are often not on PATH in spawned shells; prepend at session start:
```
# Bash
export PATH="/c/Program Files/Go/bin:/c/Users/ethan/AppData/Local/Microsoft/WinGet/Packages/BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe/mingw64/bin:$PATH"
# PowerShell
$env:Path = 'C:\Program Files\Go\bin;C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin;' + $env:Path
```
```
go build ./...
go test -race ./...             # full suite, must pass under -race
go test -race -run TestX ./pkg
go vet ./...
golangci-lint run               # binary dir: C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\GolangCI.golangci-lint_Microsoft.Winget.Source_8wekyb3d8bbwe\golangci-lint-2.11.4-windows-amd64\
make bench                      # see docs/BENCHMARKS.md
```

## Layout (full tour: `ARCHITECTURE.md`)
- `cmd/wirefan/`: server entry; `parseFlags` binds `--listen`, `--admin-addr`, `--allowed-origins`, `--dev`, `--store=sqlite|memory`, `--db-path`, `--registry=sync-map|sharded`, `--fanout=per-conn|sharded`. Env: `WIREFAN_ADMIN_TOKEN`, `WIREFAN_STATE_DIR`, `WIREFAN_TRUSTED_PROXIES`, `WIREFAN_IP_CAP`.
- `cmd/loadtest/`: load generator. `web/`: static demo assets.
- `internal/`: `conn/` per-connection state + tunables; `registry/` channel registry (sharded + syncmap, idle sweep); `hub/` broadcast; `fanout/` strategies (per-conn, sharded); `ratelimit/` token bucket; `store/` key/token store (memory + SQLite); `auth/` HMAC verify; `server/` HTTP/WS handlers, leak test; `metrics/` Prometheus.

## Conventions
- Plan-faithful by default: divergence from `docs/superpowers/plans/2026-05-04-wirefan-implementation.md` (35-task plan) needs justification noted at the diff site. One commit per task.
- Signing: single server-held HMAC secret (option b). Per-key signing (option a) was rejected; don't revive without a spec change.
- Conn constants (`sendChanSize`, ping/read deadlines, max channels) live in `internal/conn/conn.go`; change them there, not at call sites.
- Per-subscriber FIFO only: `hub.Broadcast` snapshots subscribers under `RLock`, then sends concurrently; per-conn order is held by the buffered send chan. Per-channel total ordering is NOT a protocol guarantee (broadcast mutex removed in 22fd26d to kill head-of-line blocking on slow subscribers; see `internal/hub/channel.go: Broadcast`).
- Goroutine-leak invariant is proven by `internal/server/leak_test.go`. Don't regress; new goroutine-spawning features extend that test.
- Before touching REST/HTTP/conn code, read `<vault>/projects/wirefan/project_hardening_backlog.md` (closed 2026-05-11 in `b8137fd`, every item landed) so you don't re-litigate settled decisions (e.g. `SecretHash` omitted from `GET /v1/keys`, `X-Forwarded-For` trust boundary, slow-consumer close-ordering tradeoff).

## Deferred (reopen design first)
- Redis multi-server fanout.
- Presence / membership APIs (`presence-` prefix channel-name ACL exists; member-list/join-leave events don't).

## Docs
`docs/DESIGN.md` decisions; `docs/PROTOCOL.md` wire format; `docs/BENCHMARKS.md` perf methodology + results; `docs/DEPLOY.md` Oracle Always Free runbook; `docs/superpowers/specs/2026-05-04-relay-fanout-server-design.md` locked design spec.
