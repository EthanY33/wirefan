# 04. Operability

Epic label: `epic:operability`. Tracking issue: [#98](https://github.com/EthanY33/wirefan/issues/98). Milestones: v1.1, v1.2, v1.3. Estimate: 54 h.

## Goal

Let an operator tune, observe, probe, audit, back up and install wirefan
without editing source, installing extra tools or guessing:

1. Ship the flags `docs/DESIGN.md` section 14 lists as "Not in 1.0".
2. Count every refusal and expose enough metrics to alert on.
3. Distinguish liveness from readiness.
4. Record admin actions and auth refusals in a structured log.
5. Validate configuration without booting, and give the env-only knobs flags.
6. Back up and inspect the key database with the binary itself.
7. Publish a signed container image.
8. Write down the tracing design before any tracing code returns.

## Current state

* **Hardcoded limits.** `internal/conn/conn.go` (lines 79-99):
  `defaultMaxChannelsPerConn` (64), `defaultMaxSubsPerChannel` (10 000),
  per-conn publish and control rates, `sendChanSize` (256). The 64 KiB read
  limit is `SetReadLimit` in `internal/conn/pumps.go:50`. The per-key rate
  is `ratelimit.New(100, 200, time.Hour)` in `cmd/wirefan/main.go:171`.
  `docs/DESIGN.md` 14 "Not in 1.0" lists the missing flags; `CLAUDE.md`
  "Deferred" calls them additive 1.x work.
* **Hardcoded policy.** `cmd/wirefan/main.go:181` passes
  `conn.PolicyDisconnect{}`. `PolicyDropOldest` and `PolicyDropNewest`
  (`internal/conn/policy.go`) are tested but unreachable, and record no
  `wirefan_messages_dropped_total` (`docs/PROTOCOL.md` 11 says "Not
  selectable").
* **Metric gaps.** `internal/metrics/prom.go` (lines 51-76) defines
  connections, published, dropped, latency, upgrade rejections and
  `AuthFails`. `RATE_LIMITED` and `RATE_LIMITED_CONN`
  (`internal/conn/handler.go` lines 227, 231, 253, 257) are uncounted.
  `auth.ReplayCache.Len` (`internal/auth/token.go:162`) is used only by
  tests. There is no dashboard or alert rule in the repository.
* **Health.** `internal/server/health.go` (lines 20-27) reports only
  draining. `store.Store` has no `Ping`. `deploy/Dockerfile` uses a
  distroless image with no shell or curl and no `HEALTHCHECK`.
* **Logging.** `rest.go: create` (around line 108) and `revoke` (around
  line 148) log nothing on success; `requireAdmin` (lines 79-96) returns 401
  silently. `cmd/wirefan/main.go` has no `--log-format` or `--log-level` and
  does not install a default `slog` handler.
* **Config.** `WIREFAN_IP_CAP` and `WIREFAN_TRUSTED_PROXIES` are read inside
  `internal/server/upgrade.go` (lines 109-111); `parseTrustedProxies`
  (line 153) drops malformed entries silently. There is no way to validate
  configuration without starting the server.
* **Database tooling.** `docs/DEPLOY.md` (lines 368-372 and 424-450) uses
  the `sqlite3` CLI to read `user_version` and to take backups.
  `internal/store/migrate.go` has `preflight` and versioning logic with no
  offline entry point.
* **Release.** `.github/workflows/release.yml` builds two Linux binaries and
  an unsigned `SHA256SUMS`. `docs/DEPLOY.md` (around line 591) and
  `deploy/README.md` (around line 38) describe only a local `docker build`.
* **Tracing.** Removed for 1.0 (`docs/DESIGN.md` lines 984-990, commit
  `0a8a114`); lines 1081 and 1134 defer it, and `CLAUDE.md` says to reopen
  the design before re-adding it. No `docs/rfcs/` directory exists.

## Proposed design

### Limits and policy flags (v1.1)

Introduce `conn.Limits` with `DefaultLimits()` returning today's values,
carried in `conn.Deps`, and replace the constants at their use sites in
`Run`, `handleSubscribe` (the `hub.Subscribe` max argument) and `readPump`.

| Flag | Default | Validation |
| ---- | ------- | ---------- |
| `--max-channels-per-conn` | 64 | 1 to 4096 |
| `--max-subscribers-per-channel` | 10000 | 1 to 1 000 000 |
| `--max-frame-bytes` | 65536 | 1024 to 1 048 576 |
| `--conn-publish-rate`, `--conn-publish-burst` | 50, 100 | > 0 |
| `--conn-control-rate`, `--conn-control-burst` | 20, 64 | > 0 |
| `--key-rate`, `--key-burst` | 100, 200 | > 0 |
| `--send-buffer` | 64 | 1 to 4096 |
| `--slow-consumer-policy` | `disconnect` | `disconnect`, `drop-oldest`, `drop-newest` |

Invalid values fail at startup with a message naming the flag and range.
Under the drop policies, `Policy.Apply` increments
`wirefan_messages_dropped_total` with new reason values `drop_oldest` and
`drop_newest`; ack and error frames are not counted. `docs/PROTOCOL.md` 9
gains the flag in its Source column, 11 loses "Not selectable", and
`docs/DESIGN.md` 14 records which flags shipped.

### Metrics, dashboard and alerts (v1.1)

New collectors, all additive and with bounded labels (no key ids):

| Metric | Type | Labels |
| ------ | ---- | ------ |
| `wirefan_rate_limited_total` | counter | `scope` = `conn`, `key` (later `key_ip`) and `op` = `publish`, `control` |
| `wirefan_replay_cache_entries` | gauge func | none |
| `wirefan_conn_closed_total` | counter | `reason` = `client`, `slow_consumer`, `key_revoked`, `shutdown`, `liveness`, `write_timeout`, `protocol` |
| `wirefan_build_info` | gauge (1) | `version`, `go` |

`wirefan_sign_requests_total` belongs to the sign-throttle issue in
`01-abuse-resistance.md`. New files `deploy/monitoring/grafana-wirefan.json`
and `deploy/monitoring/alerts.yml` (drop ratio, rate-limited spike,
goroutines, open file descriptors). CI checks that the dashboard JSON parses,
that every metric it references exists on `/metrics` of a test server, and
that `promtool check rules` passes.

### Readiness and healthcheck (v1.2)

`Store.Ping(ctx)` (memory returns nil; SQLite runs `SELECT 1` under a 1 s
timeout). `GET /v1/ready` returns `200 ready`, or `503 draining` /
`503 store unavailable`. `GET /v1/health` is unchanged. A `wirefan
healthcheck [--addr host:port]` subcommand GETs `/v1/ready` and exits 0 or 1,
so the distroless image can declare `HEALTHCHECK CMD ["/wirefan",
"healthcheck"]`. Subcommand dispatch in `main()` is shared with `wirefan
keys` (`06-cli-examples.md`); whichever issue lands first adds it.

### Audit log and log format (v1.2)

`internal/audit.Log(ctx, event, attrs...)` writes `slog` records with
`audit=true`. Events: `key.create`, `key.revoke`, `key.list`,
`key.rotate`, `key.kick`, `admin.auth_failed`, `sign.denied`,
`upgrade.rejected`. Attributes: key id, client IP, request id, reason.
Attacker-controlled values pass through the existing `sanitizeLogValue`.
Secrets, tokens and the admin token are never logged (asserted by a test).
`upgrade.rejected` is sampled (first N per reason per minute) so a flood
does not become a journald flood. Flags `--log-format=text|json` and
`--log-level=debug|info|warn|error` install the default handler in `main()`.

### Config flags, file and check (v1.3)

`--ip-cap` and `--trusted-proxies` flags, falling back to `WIREFAN_IP_CAP`
and `WIREFAN_TRUSTED_PROXIES`. `--config <file.json>`: a flat JSON object
whose keys are flag names, applied before command-line parsing. Precedence:
command line, then config file, then env var, then default. JSON only, to
avoid a new dependency. `--check-config` resolves everything, fails on a
malformed trusted-proxy entry or an out-of-range number, prints the resolved
configuration with secrets redacted, and exits 0 or 1. In normal runs,
dropped trusted-proxy entries are logged at warn level at boot instead of
silently.

### Database subcommands (v1.3)

| Command | Behavior |
| ------- | -------- |
| `wirefan db backup <dest>` | `VACUUM INTO` a new file with mode 0600; safe while the server runs (WAL). |
| `wirefan db check <file>` | `PRAGMA integrity_check` plus `preflight` against the binary's migrations. |
| `wirefan db version <file>` | Prints `user_version` and whether this binary can open it. |
| `wirefan db restore <src> [--db-path]` | Refuses while the target is locked by a running server or when `src` is newer than the binary; keeps the replaced file as `.bak`. |

`deploy/wirefan-backup.service` and `.timer` run a daily backup with
retention, installed by `deploy/provision.sh`. `docs/DEPLOY.md` drops the
`sqlite3` package requirement.

### Container image and signatures (v1.3)

A job in `release.yml` builds `ghcr.io/ethany33/wirefan:<tag>`, `:1.<minor>`
and `:latest` for `linux/amd64` and `linux/arm64` from `deploy/Dockerfile`
with the `VERSION` build argument. Because the binary uses cgo SQLite,
images are built natively per architecture and merged into one manifest
(the release workflow already builds natively per architecture). Provenance
and SBOM attestations are attached; the image and `SHA256SUMS` are signed
keylessly with cosign using the workflow's OIDC identity; a Trivy or Grype
scan gates the push; a hyphenated rc tag exercises the whole path as a
prerelease. The pushed image is smoke tested (`--version`, then `/v1/ready`)
before the release is marked.

### Tracing RFC (v1.3)

`docs/rfcs/0001-tracing.md`, linked from `docs/DESIGN.md` 13. Decides: span
set (upgrade, subscribe with auth check, publish with fanout recorded as
links or events rather than per-subscriber spans, sign); sampling defaults;
attribute allowlist (no payloads, no secrets, bounded cardinality); W3C
`traceparent` propagation through the WebSocket handshake and REST sign; and
the build mechanism, a `tracing` build tag so the default release binary
carries no SDK. A thin `internal/trace` interface with a no-op default lets
call sites compile either way. The deliverable includes a spike measurement:
default binary size delta (expected zero) and tagged-binary publish latency
overhead from `cmd/loadtest`.

## Protocol and API changes

| Change | Kind |
| ------ | ---- |
| Flags listed above, `--log-format`, `--log-level`, `--ip-cap`, `--trusted-proxies`, `--config`, `--check-config` | New flags, defaults equal to today's behavior |
| Subcommands `healthcheck`, `db backup/check/version/restore` | New |
| `GET /v1/ready` on the public listener | New path |
| Metrics above, `wirefan_messages_dropped_total{reason="drop_oldest\|drop_newest"}` | New metrics and label values |
| `ghcr.io/ethany33/wirefan` image | New artifact |

Example `/v1/ready` answers:

```
GET /v1/ready      ->  200 ready
GET /v1/ready      ->  503 store unavailable
GET /v1/ready      ->  503 draining
```

## Compatibility impact

Every change is an addition under `docs/COMPATIBILITY.md`: new flags and
env fallbacks whose defaults reproduce 1.0, new metrics and label values, a
new path. Existing flag-only invocations behave identically; subcommands are
recognized only when `os.Args[1]` is a known subcommand name, and no
existing flag value can be such a bare word in that position. Log wording is
not covered by the promise, but the audit event names are documented in
`docs/DEPLOY.md` so they can be relied on.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#51](https://github.com/EthanY33/wirefan/issues/51) | v1.1 | 7 h |
| 2 | [#49](https://github.com/EthanY33/wirefan/issues/49) | v1.1 | 7 h |
| 3 | [#50](https://github.com/EthanY33/wirefan/issues/50) | v1.1 | 5 h |
| 4 | [#68](https://github.com/EthanY33/wirefan/issues/68) | v1.2 | 4 h |
| 5 | [#69](https://github.com/EthanY33/wirefan/issues/69) | v1.2 | 5 h |
| 6 | [#77](https://github.com/EthanY33/wirefan/issues/77) | v1.3 | 6 h |
| 7 | [#78](https://github.com/EthanY33/wirefan/issues/78) | v1.3 | 7 h |
| 8 | [#79](https://github.com/EthanY33/wirefan/issues/79) | v1.3 | 8 h |
| 9 | [#80](https://github.com/EthanY33/wirefan/issues/80) | v1.3 | 5 h |

## Test plan

* Flags: a table test asserts every default equals the old constant; each
  invalid value fails with a readable message; handler tests prove each
  limit takes effect (`LIMIT_CHANNELS`, `LIMIT_SUBSCRIBERS`, close 1009 on
  an oversize frame, `RATE_LIMITED_CONN`).
* Policy: under `drop-oldest` and `drop-newest`, a stalled subscriber stays
  connected and the dropped counter rises by the right reason; the default
  still closes with 1008 `slow consumer`.
* Metrics: `prom_test.go` asserts each new metric; no existing name or
  label changes; dashboard and alert checks run in CI.
* Readiness: a fake store whose `Ping` fails gives 503; drain gives 503;
  `/v1/health` body and codes are unchanged; `docker inspect` reports the
  container healthy.
* Audit: `create` and `revoke` emit exactly one record each with key id and
  caller IP; failed admin auth is logged without the presented token; a
  captured-log test scans for the secret, token and admin token values.
* Config: precedence test (flag over file over env over default);
  `--check-config` exits non-zero on a malformed proxy entry and never
  prints secrets.
* Database: a backup of a live database passes `check`; `restore` refuses a
  newer `user_version`; `version` reads a v0.2.0-shaped file; the timer
  prunes old backups.
* Release: an rc tag publishes a runnable image for both architectures;
  `cosign verify` passes with the documented identity; existing binary
  assets are unchanged.

## Open questions

* Should a full sharded fanout queue keep blocking the publisher, or fail
  the publish with a new error code? Decided in the queue-depth metric issue
  in `07-performance.md` once the gauge shows how often it happens.
* `--max-subscribers-per-channel` above 10 000 is untested; the large-fanout
  benchmark in `07-performance.md` should run before the documented upper
  bound is raised.
* Should the per-channel REST attribute for the slow-consumer policy from the
  original spec follow? It needs a channel-attribute store that does not
  exist; not proposed for 1.x.
