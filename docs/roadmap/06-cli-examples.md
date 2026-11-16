# 06. CLI and examples

Epic label: `epic:cli-examples`. Tracking issue: [#100](https://github.com/EthanY33/wirefan/issues/100). Milestones: v1.2, v1.3. Estimate: 23 h.

## Goal

Replace the hand-written curl in the runbook with subcommands of the binary
operators already have, give developers a way to watch and publish to a
channel from a shell, and provide small runnable examples that show the
intended use of private channels, the stats channel and the rate limits.

## Current state

* **Entry point.** `cmd/wirefan/main.go:67` calls `parseFlags(os.Args[1:])`;
  there is no subcommand dispatch.
* **Key management.** `docs/DEPLOY.md` (around lines 280-309) creates,
  lists and revokes keys with curl and `$(sudo cat /var/lib/wirefan/admin.token)`.
  The admin routes are registered in `internal/server/rest.go` (lines 73-77).
  `resolveAdminToken` (`cmd/wirefan/main.go`, lines 202-240) resolves the
  token from `WIREFAN_ADMIN_TOKEN`, then `<state-dir>/admin.token`, and
  generates one if neither exists.
* **Shell access to channels.** `docs/PROTOCOL.md` 5.7 requires a prior
  subscribe on the same connection before publish, so curl cannot publish.
  The only Go code that speaks the protocol is the raw dial and hand-built
  frames in `cmd/loadtest/main.go` (lines 304-330).
* **Examples.** There is no `examples/` directory. `web/client.js` (1,516
  lines) is the only runnable consumer and is specific to the demo.
  `docs/PROTOCOL.md` 6 documents the read-only `_wirefan-stats` channel and
  9 documents the rate limits, with no worked example of either.

## Proposed design

### Subcommand dispatch

`main()` checks `os.Args[1]` against a fixed set of subcommand names
(`keys`, `tail`, `publish`, plus `healthcheck` and `db` from
`04-operability.md`) before calling `parseFlags`. Anything else, including
every existing flag invocation, takes the current path unchanged. Each
subcommand has its own `flag.FlagSet` and `--help`.

### `wirefan keys`

```
$ wirefan keys create --name app
id      01HKQ8M9F7XPQVJZ4YCZ7G3W2A
secret  3f6c...e1   (shown once; store it now)

$ wirefan keys list
ID                              NAME   CREATED               REVOKED
01HKQ8M9F7XPQVJZ4YCZ7G3W2A    app    2026-11-20T10:02:11Z  -

$ wirefan keys revoke 01HKQ8M9F7XPQVJZ4YCZ7G3W2A
revoked
```

Flags: `--admin-addr` (default `127.0.0.1:6060`), `--state-dir`, `--json`.
Token resolution follows `resolveAdminToken`'s order but is read-only: a
missing token is an error with the path it looked at, never a generated
token. Key fields added by `03-key-management.md` (rate, scopes, expiry,
rotate) get flags as those issues land.

### `wirefan tail` and `wirefan publish`

```
$ wirefan tail --url wss://demo.example.com --key 01HKQ8M9... demo
{"channel":"demo","id":"01HKQ8N2...","data":{"n":1}}

$ echo '{"n":2}' | wirefan publish --url wss://demo.example.com --key 01HKQ8M9... demo -
```

* `tail` prints one JSON event per line, reconnects with backoff, and exits
  on SIGINT.
* `publish` reads JSON from an argument or stdin. If `POST /v1/publish` from
  `02-protocol-api.md` exists and `--secret` is given, it uses REST;
  otherwise it opens a socket, subscribes, publishes, waits for the write to
  flush, and exits non-zero on an error frame.
* `--token-secret <id:secret>` mints a token through `/v1/auth/sign` for
  `private-` and `presence-` channels.

Implementation uses the Go client from `05-client-sdks.md` if it has landed,
otherwise a small internal helper on `coder/websocket`, as `cmd/loadtest`
does, to avoid a cross-module dependency.

### `examples/`

| Example | Shows |
| ------- | ----- |
| `examples/chat` | Node backend with a signing route (using `@wirefan/client/server` from `05-client-sdks.md`) and a browser page on a `private-room-*` channel through `authorize`. The key secret never reaches the browser. |
| `examples/dashboard` | A static page subscribed to `_wirefan-stats`, plotting connections, channels, published and dropped from the payload fields in `docs/PROTOCOL.md` 6. |
| `examples/cursors` | Multiplayer cursors throttled to 20 Hz per tab with coalescing, with a README showing the budget math against the per-connection and per-key limits in `docs/PROTOCOL.md` 9. |

Each example has its own README, starts with one command against `wirefan
--dev --allowed-origins=*`, and avoids a build step where possible. A CI job
builds the server, boots it, and runs a headless smoke script per example:
connect, subscribe, receive one event.

## Protocol and API changes

None on the wire. New CLI surface:

| Command | Kind |
| ------- | ---- |
| `wirefan keys create/list/revoke` | New subcommand |
| `wirefan tail`, `wirefan publish` | New subcommands |

## Compatibility impact

Additive under `docs/COMPATIBILITY.md` ("Minor releases may add flags").
Running the server with flags only is unchanged and covered by the existing
`cmd/wirefan/main_test.go`. Examples are documentation and carry no promise.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#75](https://github.com/EthanY33/wirefan/issues/75) | v1.2 | 6 h |
| 2 | [#86](https://github.com/EthanY33/wirefan/issues/86) | v1.3 | 5 h |
| 3 | [#87](https://github.com/EthanY33/wirefan/issues/87) | v1.3 | 12 h |

Issue 3's chat example follows
[#83](https://github.com/EthanY33/wirefan/issues/83).

## Test plan

* `keys`: `httptest` server mounting `server.RegisterAdmin`; create prints
  id and secret and the key can connect; list never prints secrets; revoke
  closes live sockets with 1008 `key revoked`; a missing token file is a
  clear error; existing `main_test.go` cases pass.
* `tail` and `publish`: in-process server; what `publish` sends appears in
  `tail`; publishing to a `_` channel exits non-zero with the
  `RESERVED_CHANNEL` message; a private channel works with `--token-secret`.
* Examples: CI smoke job per example; the cursors example runs two tabs for
  60 s with zero `RATE_LIMITED` errors.

## Open questions

* Should `wirefan keys` also talk to a remote admin listener over an SSH
  tunnel helper, or leave tunnelling to the operator? Proposed: leave it.
* Should the examples be pinned to a published `@wirefan/client` version or
  to the workspace copy? Workspace until the npm publish issue lands, then
  the published version.
