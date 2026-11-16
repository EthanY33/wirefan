# Roadmap

This file lists the work planned for wirefan after v1.0.1. Every item is
grounded in a specific file, function or doc passage in this repository,
and each one has a GitHub issue with a problem statement, a proposed
approach, acceptance criteria and an estimate.

Estimates are hours for one developer using an AI coding assistant,
including tests and doc updates. Wall-clock time for long benchmark and
soak runs is not counted.

## How the roadmap is organized

* **Milestones** are releases. v1.1, v1.2 and v1.3 are 1.x minor
  releases and contain only additive changes, as
  [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md) requires: new flags,
  env vars, endpoints, optional frame fields, frame types, error codes,
  metrics and label values, never a removal, rename or change of meaning.
  v2.0 collects changes that break that promise. It has no issues yet.
* **Epics** group related issues. Each epic has a plan document under
  [`docs/roadmap/`](docs/roadmap/) with the current state, the proposed
  design, protocol or API changes, compatibility impact, the task
  breakdown and a test plan.
* **Labels** follow `area:*` (which part of the tree), `kind:*` (what
  sort of work) and `epic:<slug>`. A few small, well-scoped issues carry
  `good first issue`.

Each epic also has a tracking issue labelled `epic` that checks off its
child issues as they close.

### Items listed as out of scope for 1.x

README, CONTRIBUTING and `docs/COMPATIBILITY.md` ("Scope of 1.x") list
presence member lists, message history and replay, multi-process fanout
and publishing `@wirefan/client` to npm as out of scope for 1.x. Such an
item moves into a 1.x milestone only where the change is purely additive:

* **npm publish moves into v1.2.** The JS client is versioned separately
  (0.x) and is not covered by the server's 1.x promise, so publishing it
  changes no server contract. The issue removes the "not on npm yet" and
  "Out of scope" lines where they appear.
* **Presence lists, history and multi-process fanout stay out of 1.x.**
  `docs/DESIGN.md` section 14 states that each one changes the design
  rather than adding to it. They are listed under [Not planned](#not-planned).

## Milestones

### v1.1: abuse resistance and the deferred 1.0 flags (79 h)

Closes the known nuisance items on the public demo (one visitor starving
the shared demo key, host-level abuse, open SSH), tightens the public
auth surface, ships the flags that `docs/DESIGN.md` section 14 lists as
"Not in 1.0", fills the metric gaps, and lays the test and measurement
foundations that later performance work depends on.

| Title | Issue | Epic | Est. |
| ----- | ----- | ---- | ---- |
| fix(server): bind /v1/auth/sign socket_id to a socket opened with the calling key | [#43](https://github.com/EthanY33/wirefan/issues/43) | abuse-resistance | 4 h |
| feat(server): throttle /v1/auth/sign and failed admin or key auth per source IP | [#44](https://github.com/EthanY33/wirefan/issues/44) | abuse-resistance | 5 h |
| feat(ratelimit): per-IP sub-limit inside a shared key's publish budget | [#45](https://github.com/EthanY33/wirefan/issues/45) | abuse-resistance | 6 h |
| ops(demo): SSH and host abuse hardening for the demo VM (key-only sshd drop-in, fail2ban, IAP-only 22, nftables connection limits) | [#46](https://github.com/EthanY33/wirefan/issues/46) | abuse-resistance | 6 h |
| ops(systemd): tighten wirefan.service to a measured systemd-analyze security score | [#47](https://github.com/EthanY33/wirefan/issues/47) | abuse-resistance | 3 h |
| fix(conn): enforce text-only inbound frames as PROTOCOL.md 2 requires | [#48](https://github.com/EthanY33/wirefan/issues/48) | protocol-api | 2 h |
| feat(server): make resource limits configurable by flag (channels per conn, subscribers per channel, frame size, rates) | [#49](https://github.com/EthanY33/wirefan/issues/49) | operability | 7 h |
| feat(conn): select slow-consumer policy with --slow-consumer-policy and count drop-policy drops | [#50](https://github.com/EthanY33/wirefan/issues/50) | operability | 5 h |
| feat(metrics): fill Prometheus metric gaps and ship a Grafana dashboard with alert rules | [#51](https://github.com/EthanY33/wirefan/issues/51) | operability | 7 h |
| test(perf): add Go microbenchmarks for the publish hot path and track them in CI with benchstat | [#52](https://github.com/EthanY33/wirefan/issues/52) | performance | 6 h |
| fix(loadtest): count dropped latency samples and report percentiles from a proper histogram | [#53](https://github.com/EthanY33/wirefan/issues/53) | performance | 5 h |
| test(conn): fuzz the inbound frame handler, channel-name check and frame encoder | [#54](https://github.com/EthanY33/wirefan/issues/54) | testing | 4 h |
| test(auth): fuzz and property-test subscribe-token sign/verify | [#55](https://github.com/EthanY33/wirefan/issues/55) | testing | 3 h |
| test(server): sharded-fanout shutdown, drain and revoke race tests | [#56](https://github.com/EthanY33/wirefan/issues/56) | testing | 4 h |
| chore(lint): add a .golangci.yml and enable linters that fit a network server | [#57](https://github.com/EthanY33/wirefan/issues/57) | testing | 4 h |
| ci: coverage floors per package plus a nightly shuffle/count flake run | [#58](https://github.com/EthanY33/wirefan/issues/58) | testing | 5 h |
| test(ci): run loadtest and bench.sh logic against a real server on every PR | [#59](https://github.com/EthanY33/wirefan/issues/59) | testing | 3 h |

### v1.2: key management, socketless publish and client release (105 h)

Gives API keys their own budgets, scopes, expiry and secret rotation
(all through appended schema migrations), adds a REST publish path for
app servers and an opt-in publish ack, publishes `@wirefan/client` to npm,
and adds a key-management CLI so the runbook no longer needs curl.

| Title | Issue | Epic | Est. |
| ----- | ----- | ---- | ---- |
| feat(ratelimit): per-key rate overrides stored with the API key | [#60](https://github.com/EthanY33/wirefan/issues/60) | key-management | 8 h |
| feat(auth): key scopes (publish-only, subscribe-only) and channel-prefix allowlists | [#61](https://github.com/EthanY33/wirefan/issues/61) | key-management | 10 h |
| feat(admin): key expiry (expires_at) enforced at upgrade and sign time | [#62](https://github.com/EthanY33/wirefan/issues/62) | key-management | 6 h |
| feat(admin): per-key stats and a non-sticky connection kick endpoint | [#63](https://github.com/EthanY33/wirefan/issues/63) | key-management | 6 h |
| feat(auth): zero-downtime API key secret rotation with an overlap window | [#64](https://github.com/EthanY33/wirefan/issues/64) | key-management | 6 h |
| docs: publish an OpenAPI 3.1 spec for the REST API and check it against the router in CI | [#65](https://github.com/EthanY33/wirefan/issues/65) | protocol-api | 5 h |
| feat(server): add POST /v1/publish so app servers can publish without a socket | [#66](https://github.com/EthanY33/wirefan/issues/66) | protocol-api | 8 h |
| feat(protocol): optional publish ack carrying the server-assigned event id | [#67](https://github.com/EthanY33/wirefan/issues/67) | protocol-api | 6 h |
| feat(server): add /v1/ready readiness probe that checks the store, and a wirefan healthcheck subcommand | [#68](https://github.com/EthanY33/wirefan/issues/68) | operability | 4 h |
| feat(ops): structured audit log for admin actions and auth rejections, plus --log-format=json | [#69](https://github.com/EthanY33/wirefan/issues/69) | operability | 5 h |
| test(web): headless-browser smoke test of the embedded demo page in CI | [#70](https://github.com/EthanY33/wirefan/issues/70) | testing | 8 h |
| feat(client-js): stop reconnecting on terminal server answers (key revoked, bad key) | [#71](https://github.com/EthanY33/wirefan/issues/71) | client-sdks | 6 h |
| feat(client-js): typed channel payloads and a type-checked test suite | [#72](https://github.com/EthanY33/wirefan/issues/72) | client-sdks | 5 h |
| perf(client-js): minified bundle, script-tag build and a gzip size budget in CI | [#73](https://github.com/EthanY33/wirefan/issues/73) | client-sdks | 4 h |
| ci(client-js): publish @wirefan/client to npm with provenance from a tagged release | [#74](https://github.com/EthanY33/wirefan/issues/74) | client-sdks | 6 h |
| feat(cmd/wirefan): add `wirefan keys` create, list and revoke admin subcommands | [#75](https://github.com/EthanY33/wirefan/issues/75) | cli-examples | 6 h |
| ci: add Windows, macOS and arm64 jobs to the PR test matrix | [#76](https://github.com/EthanY33/wirefan/issues/76) | testing | 6 h |

### v1.3: SDK breadth, release artifacts and measured performance (126 h)

Adds a Go client, React hooks, a server-side auth helper and runnable
examples, publishes a signed container image, and runs the performance
work that v1.1's measurement fixes make trustworthy: large-channel and
multi-core benchmarks, allocation removal on the publish path, an idle
memory curve, a compression spike and a long soak.

| Title | Issue | Epic | Est. |
| ----- | ----- | ---- | ---- |
| feat(config): --ip-cap and --trusted-proxies flags, a JSON config file, and --check-config | [#77](https://github.com/EthanY33/wirefan/issues/77) | operability | 6 h |
| feat(store): wirefan db subcommands for backup, restore, integrity check and migration status | [#78](https://github.com/EthanY33/wirefan/issues/78) | operability | 7 h |
| ci(release): publish multi-arch container image to GHCR with SBOM and provenance, and sign release checksums | [#79](https://github.com/EthanY33/wirefan/issues/79) | operability | 8 h |
| docs(design): RFC for opt-in OpenTelemetry tracing behind a build tag | [#80](https://github.com/EthanY33/wirefan/issues/80) | operability | 5 h |
| feat(client-js): reconnect promptly on browser online, pageshow and tab-visible events | [#81](https://github.com/EthanY33/wirefan/issues/81) | client-sdks | 4 h |
| feat(client-js): opt-in bounded publish outbox while reconnecting | [#82](https://github.com/EthanY33/wirefan/issues/82) | client-sdks | 5 h |
| feat(client-js): server-side helper entry point for private-channel auth endpoints | [#83](https://github.com/EthanY33/wirefan/issues/83) | client-sdks | 5 h |
| feat(clients/react): @wirefan/react hooks (provider, useChannel, useConnectionState, usePublish) | [#84](https://github.com/EthanY33/wirefan/issues/84) | client-sdks | 8 h |
| feat(clients/go): official Go client module with reconnect and resubscribe | [#85](https://github.com/EthanY33/wirefan/issues/85) | client-sdks | 14 h |
| feat(cmd/wirefan): add `wirefan tail` and `wirefan publish` debugging subcommands | [#86](https://github.com/EthanY33/wirefan/issues/86) | cli-examples | 5 h |
| docs(examples): runnable examples directory (private-channel chat, live stats dashboard, multiplayer cursors) | [#87](https://github.com/EthanY33/wirefan/issues/87) | cli-examples | 12 h |
| feat(metrics): expose sharded fanout queue depth and enqueue wait | [#88](https://github.com/EthanY33/wirefan/issues/88) | performance | 3 h |
| bench: hot-channel and large-fanout scenarios in loadtest | [#89](https://github.com/EthanY33/wirefan/issues/89) | performance | 6 h |
| bench: idle-connection memory curve (RSS and goroutines per connection) from 1k to 20k | [#90](https://github.com/EthanY33/wirefan/issues/90) | performance | 6 h |
| perf(hub,conn): remove per-publish allocations in hub.Broadcast, marshalFrame and ShardedPool.shardFor | [#91](https://github.com/EthanY33/wirefan/issues/91) | performance | 8 h |
| bench: multi-core scaling runs for per-conn vs sharded fanout | [#92](https://github.com/EthanY33/wirefan/issues/92) | performance | 8 h |
| spike(conn): measure permessage-deflate and add opt-in --compression flag if it pays | [#93](https://github.com/EthanY33/wirefan/issues/93) | performance | 8 h |
| test(soak): long-running churn soak with heap, goroutine and fd tracking | [#94](https://github.com/EthanY33/wirefan/issues/94) | testing | 8 h |

### v2.0: breaking changes (no issues yet)

No planned item requires a breaking change. This milestone exists so
that a change which would break `docs/COMPATIBILITY.md` has a place to
go instead of landing in a minor. An item enters it only with an accepted
design document, as `docs/DESIGN.md` section 14 requires for anything on
its non-goals list.

Candidates recorded in the plan documents, not yet issues:

* Make `--sign-same-key-only` the default, so `/v1/auth/sign` refuses to
  sign for a socket opened with a different key
  ([01-abuse-resistance.md](docs/roadmap/01-abuse-resistance.md)).
* Turn on the per-IP sub-limit inside a key by default
  ([01-abuse-resistance.md](docs/roadmap/01-abuse-resistance.md)).

**Total: 52 issues, 310 hours.**

## Epics

| Epic | Plan | Issues | Hours |
| ---- | ---- | ------ | ----- |
| `abuse-resistance` | [01-abuse-resistance.md](docs/roadmap/01-abuse-resistance.md) | 5 | 24 |
| `protocol-api` | [02-protocol-api.md](docs/roadmap/02-protocol-api.md) | 4 | 21 |
| `key-management` | [03-key-management.md](docs/roadmap/03-key-management.md) | 5 | 36 |
| `operability` | [04-operability.md](docs/roadmap/04-operability.md) | 9 | 54 |
| `client-sdks` | [05-client-sdks.md](docs/roadmap/05-client-sdks.md) | 9 | 57 |
| `cli-examples` | [06-cli-examples.md](docs/roadmap/06-cli-examples.md) | 3 | 23 |
| `performance` | [07-performance.md](docs/roadmap/07-performance.md) | 8 | 50 |
| `testing` | [08-testing.md](docs/roadmap/08-testing.md) | 9 | 45 |

**abuse-resistance.** The public listener has three soft spots: every
connection on a key spends one shared publish bucket
(`internal/conn/handler.go`, `cmd/wirefan/main.go: run`), so one source IP
on the public demo key can starve every other visitor; `/v1/auth/sign`
has no limiter and does not check that `socket_id` belongs to the calling
key (`internal/server/rest.go: sign`, admitted in `docs/DESIGN.md`
section 5); and the demo host's SSH, fail2ban and connection-limit setup
lives nowhere in the repository. This epic closes those gaps and
measures the systemd sandbox.

**protocol-api.** Publishing requires an open, subscribed WebSocket
(`docs/PROTOCOL.md` 5.7), a successful publish is never acknowledged, and
binary frames are processed as JSON despite the text-only rule in
`docs/PROTOCOL.md` 2. This epic adds `POST /v1/publish`, an opt-in
`published` ack, the text-only check, and an OpenAPI spec held in sync
with the router by a test.

**key-management.** `store.Key` (`internal/store/store.go`) has only an
id, name, secret hash and revocation time. This epic adds per-key rate
overrides, scopes and channel-prefix allowlists, expiry, secret rotation
with an overlap window, and admin stats and kick endpoints. Each schema
change is an appended migration in `internal/store/migrate.go`.

**operability.** Limits and the slow-consumer policy are compile-time
constants, the store is never probed for readiness, admin actions leave
no audit trail, several failure paths are uncounted in Prometheus, and
the release ships only binaries and unsigned checksums. This epic adds
the flags, probes, audit log, metrics, database tooling and signed
container image, plus a written RFC before tracing returns.

**client-sdks.** The JS client retries forever on terminal answers such
as a revoked key, reconnects only on a timer, throws on publish while
reconnecting, has untyped payloads and is not on npm. This epic fixes
those, adds a server-side signing helper and React hooks, and adds an
official Go client.

**cli-examples.** Key management in `docs/DEPLOY.md` is hand-written
curl, there is no way to watch or publish to a channel from a shell, and
the only runnable consumer is the demo page. This epic adds `wirefan
keys`, `wirefan tail` and `wirefan publish`, and an `examples/` directory
with a CI smoke job.

**performance.** No `testing.B` benchmark exists, the load generator
silently drops latency samples, and every published number uses 10
subscribers per channel on one CPU. This epic fixes the measurement
first, then measures the cases the design is most likely to fail at
(hot channels, many cores, many idle connections) and removes
allocations on the publish path only where a benchmark shows a gain.

**testing.** There are no fuzz targets, no coverage measurement, no
repeated-run flake detection, no non-Linux CI, no browser test of the
demo page and no long soak. The sharded fanout's shutdown ordering is
documented in `internal/server/server.go` but untested end to end. This
epic adds each of those.

## Not planned

These items are excluded on purpose. Reopening one needs a design
document first, as `docs/DESIGN.md` section 14 requires.

| Item | Reason |
| ---- | ------ |
| Multi-process fanout (Redis or otherwise) | `docs/COMPATIBILITY.md` "Scope of 1.x" and `docs/DESIGN.md` 14: a design change, not an addition. Sticky versus stateless routing is an open question (`docs/DESIGN.md` 13). |
| Presence member lists and join/leave events | `docs/DESIGN.md` 13 says not to decide the presence model until there is a real first user; 14 says it does not land in 1.x. |
| Message history and replay, and session resume built on it | `docs/DESIGN.md` 12.3 and 14: changes the design rather than adding to it, and collides with the registry sweeper. |
| Per-key token signing secrets | `docs/DESIGN.md` 5.3 rejected option (a); a single server-held secret per boot is the settled 1.x design. |
| Persisting the token signing secret across restarts | Tokens are bound to `socket_id` and a restart drops every socket, so no pre-restart token could be reused. It would add a secret at rest for no gain. |
| Per-key channel namespacing for tenants | Server-wide channel names are documented behavior (`docs/PROTOCOL.md` 6) for a single-operator server, with no use case asking for isolation. Key scopes with channel-prefix allowlists cover the access-control part. |
| Channel occupied/vacated webhooks | A new outbound subsystem (delivery, retries, signing) with no doc passage or user need behind it. |
| WebTransport / HTTP/3, MessagePack or Protobuf wire formats, a custom epoll loop | `docs/DESIGN.md` 8.3, 8.4 and 14: deliberate non-goals. |
| Pusher-protocol compatibility | `docs/DESIGN.md` 13 and 14: would creep the spec. |
| Exactly-once or ordered delivery across reconnects | `docs/DESIGN.md` 14. The opt-in client outbox narrows loss during a reconnect but keeps at-most-once semantics. |
