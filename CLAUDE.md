# CLAUDE.md — wirefan project memory

Project memory for future Claude Code sessions. Read this first.

## What this is

Single-binary Go WebSocket fanout server (`wirefan`). Channel-based pub/sub with HMAC-signed subscribe tokens. Targets ~$0 hosting on Oracle Always Free.

## Build / test commands

PATH must include Go and mingw gcc (CGO required for `-race`). See **Windows gotchas** below.

```
# Bash (Git Bash / WSL-style)
export PATH="/c/Program Files/Go/bin:/c/Users/ethan/AppData/Local/Microsoft/WinGet/Packages/BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe/mingw64/bin:$PATH"

# PowerShell
$env:Path = 'C:\Program Files\Go\bin;C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin;' + $env:Path
```

After PATH is set:

```
go build ./...                  # build all
go test -race ./...             # full suite (must pass under -race)
go test -race -run TestX ./pkg  # single test
go vet ./...
golangci-lint run               # see Windows gotchas for lint binary path
make bench                      # benchmark suite (see docs/BENCHMARKS.md)
```

## Code organization

See `ARCHITECTURE.md` for the full tour. Quick pointers:

- `cmd/wirefan/` — server entry; `parseFlags` binds `--listen`, `--admin-addr`, `--allowed-origins`, `--dev`
- `cmd/loadtest/` — load generator
- `internal/conn/` — per-connection state; tunable constants live here
- `internal/hub/` — channel registry + broadcast
- `internal/auth/` — HMAC token verify
- `internal/server/` — HTTP/WS handlers, leak test
- `internal/metrics/` — Prometheus
- `web/` — static demo assets

## Repo conventions (non-obvious)

- All tests pass under `-race`. CGO required (mingw on Windows).
- **Plan-faithful by default** — divergence from `docs/superpowers/plans/2026-05-04-wirefan-implementation.md` requires justification noted at the diff site.
- **Server-side signing secret architecture (option b)** — single server-held HMAC secret; **NOT** per-key signing (option a was explicitly rejected).
- Conn-level constants (`sendChanSize`, ping/read deadlines, max channels) live in `internal/conn/conn.go`. Change them there, not at call sites.
- **Per-subscriber FIFO only** — `hub.Broadcast` snapshots subscribers under `RLock` then sends concurrently; ordering is preserved per-conn by the buffered send chan. Per-channel total ordering is **not** a protocol guarantee (`BroadcastMu` was removed in 22fd26d to kill head-of-line blocking on slow subscribers — see `internal/hub/channel.go:42-50`).
- **Goroutine-leak invariant** is proven by `internal/server/leak_test.go`. Do not regress; if a new feature spawns goroutines, add to that test.

## Active hardening backlog

Deferred follow-ups from per-task code reviews are tracked in the ethan-memory Obsidian vault at:

```
<vault>/projects/wirefan/project_hardening_backlog.md
```

Junctioned from `~/.claude/projects/C--Users-ethan-Desktop-Projects-wirefan/memory/` so it auto-loads when a session starts in this repo.

Examples currently on it: `SecretHash` exposure in `GET /v1/keys`, `X-Forwarded-For` honoring for per-IP cap, etc. Consult before starting new work to avoid duplicating fixes.

## Deferred (do not implement without reopening design)

- Per-key signing (option a) — rejected; do not revive without spec change
- Redis multi-server fanout
- Presence / membership APIs (channel-name ACL exists for `presence-` prefix, but member-list/join-leave events are not implemented)

## Windows-dev-environment gotchas

- Go binary: `C:\Program Files\Go\bin\go.exe` — often **not** on PATH for spawned shells. Prepend it.
- gcc (mingw via WinLibs): `C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin\` — required for `-race` (CGO).
- golangci-lint: `C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\GolangCI.golangci-lint_Microsoft.Winget.Source_8wekyb3d8bbwe\golangci-lint-2.11.4-windows-amd64\`
- See PATH-prepend snippets above; copy/paste them at session start.

## Live links

- Demo: _TBD post-deploy_
- Repo: _TBD post-publish_

## Commit conventions

- Conventional Commit prefixes: `feat`, `fix`, `docs`, `test`, `chore`, `refactor`
- One commit per task per the implementation plan
- **No `Co-Authored-By` trailers** — none of the existing commits have them; preserve the pattern.

## Doc map

- `ARCHITECTURE.md` — onboarding navigation
- `docs/DESIGN.md` — architectural decisions
- `docs/PROTOCOL.md` — wire format
- `docs/BENCHMARKS.md` — perf methodology + results
- `docs/DEPLOY.md` — Oracle Always Free runbook
- `docs/superpowers/specs/2026-05-04-relay-fanout-server-design.md` — locked design spec
- `docs/superpowers/plans/2026-05-04-wirefan-implementation.md` — 35-task plan
