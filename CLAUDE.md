# wirefan

Single-binary Go WebSocket fanout server: channel pub/sub, HMAC-signed subscribe tokens. Targets a single small Ubuntu 24.04 VPS from any provider. Repo: https://github.com/EthanY33/wirefan. Demo: TBD post-deploy.

## Build / test
CGO is required for a working binary (the default SQLite store) and for the full test suite, not just `-race` (`internal/store/sqlite.go` uses `mattn/go-sqlite3`); with `CGO_ENABLED=0` the build succeeds but the SQLite store fails at startup. Go (`C:\Program Files\Go\bin`) and mingw gcc (WinLibs) are often not on PATH in spawned shells; prepend at session start:
```
# Bash
export PATH="/c/Program Files/Go/bin:/c/Users/ethan/AppData/Local/Microsoft/WinGet/Packages/BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe/mingw64/bin:$PATH"
# PowerShell
$env:Path = 'C:\Program Files\Go\bin;C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT.LLVM_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin;' + $env:Path
```
```
go build ./...
go test -race ./...             # full suite, must pass under -race (what CI runs)
go test -short -race ./...      # skips internal/conn's 65 s production-timer keepalive test
go test -race -run TestX ./pkg
go vet ./...
golangci-lint run               # binary dir: C:\Users\ethan\AppData\Local\Microsoft\WinGet\Packages\GolangCI.golangci-lint_Microsoft.Winget.Source_8wekyb3d8bbwe\golangci-lint-2.11.4-windows-amd64\
cd clients/js && npm ci && npm test   # JS client; after editing src, `npm run vendor:web` (CI fails on a stale web/wirefan-client.js)
make bench                      # needs Docker + bash; see docs/BENCHMARKS.md
```
`go.mod` requires at least `toolchain go1.26.8`; an older local Go downloads it on first use.

## Layout (full tour: `ARCHITECTURE.md`)
- `cmd/wirefan/`: server entry; `parseFlags` binds `--listen`, `--admin-addr`, `--allowed-origins`, `--dev`, `--store=sqlite|memory`, `--db-path`, `--registry=sync-map|sharded`, `--fanout=per-conn|sharded`, `--version`. Env: `WIREFAN_ADMIN_TOKEN`, `WIREFAN_STATE_DIR`, `WIREFAN_TRUSTED_PROXIES`, `WIREFAN_IP_CAP`.
- `cmd/loadtest/`: load generator. `web/`: embedded demo assets (`wirefan-client.js` is a vendored build of `clients/js`, never hand-edit).
- `internal/`: `conn/` per-connection state, pumps, keepalive, tunables, exported `ValidateChannelName`/`ChannelRequiresAuth`; `registry/` channel registry (sharded + syncmap, empty-channel sweep); `hub/` live-conn set (`Add`/`Len`/`CloseKey`/`Drain`) + channel Subscribe/Broadcast + stats publisher; `fanout/` strategies (per-conn, sharded); `ratelimit/` per-key token bucket; `store/` API-key store (memory + SQLite, versioned migrations); `auth/` secret gen/hash, HMAC token sign/verify, jti replay cache; `server/` HTTP/WS handlers, leak tests; `metrics/` Prometheus (no OTel).
- `clients/js/`: `@wirefan/client` TS SDK (own 0.x version). `deploy/`: provision.sh, deploy.sh, systemd unit, Caddyfile, Dockerfile.

## Conventions
- Plan-faithful by default: divergence from `docs/superpowers/plans/2026-05-04-wirefan-implementation.md` (35-task plan) needs justification noted at the diff site. One commit per task. Known divergences from the spec's locked-in features include: OTel hook removed (feature 7); slow-consumer policy and resource limits hardcoded, no flags (features 4, 8; see Deferred); keepalive retuned to a 10 s pong wait with no read deadline (feature 5); sharded pool sized to `GOMAXPROCS`, not NumCPU x2 (feature 9).
- 1.x is frozen by `docs/COMPATIBILITY.md` (protocol, HTTP API, flags, env vars, metric names, key DB): add, never remove/rename/change meaning. `internal/` and the subscribe-token layout are not covered.
- Signing: single server-held HMAC secret, generated per boot (option b). Per-key signing (option a) was rejected; don't revive without a spec change.
- Conn tunables live in `internal/conn/conn.go`; change them there, not at call sites. Consts: `sendChanSize`, `writeDeadline`, channel/subscriber caps, per-conn rate limits. Package vars (tests shorten them): `pingInterval` (30 s), `pongWait` (10 s), `closeHandshakeTimeout`. There is no read deadline: pongs are handled inside `Read` and never reset one. Exceptions: the 64 KiB inbound message cap is set in `pumps.go: readPump` and the channel-name cap `maxChannelNameLen` is in `handler.go`.
- Rate limits: per-conn buckets (publish 50/s burst 100; subscribe/unsubscribe 20/s burst 64) are charged before the shared per-key bucket (100/s burst 200, `cmd/wirefan/main.go: run`), so a request a per-conn limit rejects spends no key budget.
- Frames built in `internal/conn/handler.go` (client events, acks, errors) go through `marshalFrame` (no HTML escaping, so relayed payloads are not inflated), not `json.Marshal`. The hello (`conn.go`) and `_wirefan-stats` events (`hub/stats.go`) still use `json.Marshal`; they carry no client data.
- Per-subscriber FIFO only: `hub.Broadcast` snapshots subscribers under `RLock`, then calls `Send` on each in turn with no per-channel lock, so under `--fanout=per-conn` concurrent publishes to one channel are not serialized (sharded's one worker per channel does serialize them); per-conn order is held by the buffered send chan. Per-channel total ordering is NOT a protocol guarantee (per-channel broadcast mutex removed in 22fd26d; see `internal/hub/channel.go: Broadcast`).
- Goroutine-leak invariant is proven by `internal/server/leak_test.go` (conn churn under both fanouts; `Hub.CloseKey`/`Hub.Drain`). Don't regress; new goroutine-spawning features extend that test.
- Admission barriers: `Hub.Add` refuses conns once `Drain` started (1001) or `CloseKey` ran for the key (1008), checked under the lock those snapshot under; `/v1/connect` answers 503 while draining. Frames that are not valid UTF-8 get `BAD_JSON` before unmarshalling (RFC 6455: browsers fail on invalid UTF-8 in text frames).
- Before touching REST/HTTP/conn code, read `<vault>/projects/wirefan/project_hardening_backlog.md` (closed 2026-05-11 in `b8137fd`, every item landed) so you don't re-litigate settled decisions (e.g. `SecretHash` omitted from `GET /v1/keys`, `X-Forwarded-For` trust boundary, slow-consumer close-ordering tradeoff).

## Deferred (reopen design first)
Out of scope for 1.x (`docs/COMPATIBILITY.md`, "Scope of 1.x"): multi-process fanout (Redis or otherwise); presence membership (`presence-` needs a token today, but there are no member lists or join/leave events); message history. Also out of scope for 1.x per README/CONTRIBUTING: history replay, per-key signing secrets, publishing `@wirefan/client` to npm. OTel tracing was removed for 1.0; reopen design before re-adding it. Hardcoded with no flag yet (adding one is an additive 1.x change): backpressure policy (`PolicyDisconnect` in `main.go: run`), per-conn limits and caps (`conn.go`), per-key rate (`main.go: run`).

## Docs
`docs/DESIGN.md` decisions; `docs/PROTOCOL.md` wire format; `docs/COMPATIBILITY.md` 1.x SemVer promise; `docs/BENCHMARKS.md` perf methodology + results; `docs/DEPLOY.md` provider-agnostic Ubuntu 24.04 VPS runbook (provision.sh/deploy.sh, Docker and Cloudflare appendices); `clients/js/README.md` JS client; `docs/superpowers/specs/2026-05-04-relay-fanout-server-design.md` locked design spec.
