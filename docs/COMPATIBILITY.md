# Compatibility promise

wirefan follows [Semantic Versioning](https://semver.org). This page says
exactly what the version number protects, so you can upgrade within 1.x
without reading every diff.

In short: anything a client, an app server, an operator script or a
dashboard can observe from outside the process is covered. A change that
would break one of them waits for 2.0. Internals are not covered.

## Covered in 1.x

A **patch** release (1.0.x) only fixes bugs. A **minor** release (1.x.0)
may add things. Neither removes or renames anything below, or changes what
it means.

### Wire protocol v1

Specified in [`PROTOCOL.md`](PROTOCOL.md).

- The `/v1/connect` endpoint and the `connected.version` value `"v1"`.
- Frame types and their fields: `connected`, `subscribe`, `subscribed`,
  `unsubscribe`, `unsubscribed`, `publish`, `event`, `error`.
- Error `code` values and WebSocket close codes.
- Channel naming rules, including the `private-`, `presence-` and `_`
  prefixes.
- The keys of `_wirefan-stats` event payloads.
- The subscribe token is an opaque string. Its internal layout may change
  in any release; only wirefan produces and reads it.

Minor releases may add optional fields to frames, new error codes and new
frame types. Clients must ignore fields they do not recognize and treat an
unknown error code as a failure of the operation it answers. A breaking
protocol change would ship as `v2` on a new path (`/v2/connect`) in a 2.0
release, with a published schedule for retiring `v1`.

### HTTP API

- Paths, methods and authentication schemes of `/v1/health`,
  `/v1/auth/sign` and `/v1/keys`.
- Status codes and JSON field names of their responses. Minor releases may
  add response fields.
- `/metrics` and `/debug/pprof/*` stay on the admin listener.

### Command line and environment

- Every flag: `--listen`, `--admin-addr`, `--allowed-origins`, `--dev`,
  `--store`, `--db-path`, `--registry`, `--fanout`, `--version`.
- Every environment variable: `WIREFAN_ADMIN_TOKEN`, `WIREFAN_STATE_DIR`,
  `WIREFAN_TRUSTED_PROXIES`, `WIREFAN_IP_CAP`.
- Their default values.

Minor releases may add flags and variables.

### Prometheus metrics

- Metric names and label names exposed on `/metrics` under the
  `wirefan_` prefix. Minor releases may add metrics and label values.
- Go runtime and process metrics come from the Prometheus client library
  and follow its versioning, not wirefan's.

### Data on disk

- A key database written by any 1.x release opens in every later 1.x
  release. Schema upgrades run automatically at startup, inside a
  transaction, and never lose keys.
- Downgrades are not supported once a newer release has migrated the
  schema: an older binary refuses a database newer than it understands,
  rather than guessing. `deploy/deploy.sh` snapshots the database before an
  upgrade and restores it on an automatic rollback.
- The admin token file format (one line, the token) does not change.

## Not covered

- Go packages under `internal/`. They cannot be imported from outside the
  module and change freely.
- Log line wording and structure.
- Performance characteristics. Numbers in
  [`BENCHMARKS.md`](BENCHMARKS.md) describe one measured setup.
- The embedded demo page served at `/`.
- The `@wirefan/client` JavaScript API in `clients/js`, which has its own
  0.x version and may change in a minor version until its own 1.0. It speaks
  wire protocol v1, so every 1.x server works with it.
- Build and deploy scripts under `deploy/` and `scripts/`, beyond what
  [`DEPLOY.md`](DEPLOY.md) documents as the supported procedure.

## Scope of 1.x

1.x is a single-process server. Running several wirefan processes behind
a load balancer with shared fanout (through Redis or otherwise), presence
membership events, and message history are design changes rather than
additions, so they are out of scope for 1.x.
