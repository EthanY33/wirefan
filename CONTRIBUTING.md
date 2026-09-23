# Contributing to wirefan

Thanks for taking the time. Bug reports, fixes, docs corrections and
benchmark runs on hardware the repo does not cover yet are all welcome.

## Before you start

- Open an issue first for anything bigger than a small fix, so the design
  can be agreed before code is written.
- Some directions are deliberately out of scope for 1.x and need a design
  discussion before any code: multi-server fanout (Redis or otherwise),
  presence and membership events, message history and replay, and per-key
  signing secrets. The reasoning is in [`docs/DESIGN.md`](docs/DESIGN.md).

## Development setup

You need Go 1.26 and a C compiler. The SQLite driver
(`mattn/go-sqlite3`) uses cgo: without `CGO_ENABLED=1` and `gcc` or `clang`
on `PATH` the code still compiles, but the binary cannot open its default
SQLite store, the store tests fail, and `-race` needs cgo anyway. On Windows, a MinGW-w64
toolchain such as WinLibs works. The JavaScript client needs Node 22 or later.

```bash
go build ./...
go vet ./...
go test -race ./...     # the whole suite must pass under the race detector
golangci-lint run       # v2.x, same version CI pins

cd clients/js
npm ci
npm test
```

`make build`, `make test-race`, `make lint` and `make bench` wrap the same
commands. `make bench` needs Docker and bash; see
[`docs/BENCHMARKS.md`](docs/BENCHMARKS.md).

## Invariants that reviews check

- **Race-clean.** `go test -race ./...` passes.
- **No goroutine leaks.** `internal/server/leak_test.go` proves the server
  returns to within a small tolerance of its goroutine baseline after
  1,000-connection churn, key revocation and shutdown. A feature
  that starts goroutines extends that test.
- **Per-subscriber FIFO only.** Each connection's buffered send channel keeps
  its own order. Per-channel total ordering is not a guarantee, and adding a
  broadcast-wide lock to get it would serialize every publish to a channel
  behind the previous one's send loop.
- **Connection tunables live in one place.** Send-buffer size, write
  deadline, channel and subscriber caps and per-connection rate limits are
  constants in `internal/conn/conn.go`; the keepalive timers `pingInterval`
  and `pongWait` are package variables there so tests can shorten them.
  There is no read deadline. Change them there, not at call sites.
- **Docs follow behavior.** A change to frames, error codes or close codes
  updates [`docs/PROTOCOL.md`](docs/PROTOCOL.md). A new package updates the
  repo map in [`ARCHITECTURE.md`](ARCHITECTURE.md). A performance claim links
  a raw run under `results/`.

## Compatibility

wirefan follows [Semantic Versioning](https://semver.org). Within 1.x, the
wire protocol (`v1`), the HTTP endpoints, the command-line flags, the
`WIREFAN_*` environment variables and the metric names only change in
backward-compatible ways; a breaking change waits for 2.0.
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md) lists exactly what is
covered.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org):
  `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `ci`, with a scope when
  it helps (`fix(store): ...`).
- Keep a pull request to one logical change, and say in the description how
  you verified it.
- Add a line under `Unreleased` in [`CHANGELOG.md`](CHANGELOG.md) for anything
  a user or operator would notice.

By contributing you agree that your work is released under the
[MIT License](LICENSE).
