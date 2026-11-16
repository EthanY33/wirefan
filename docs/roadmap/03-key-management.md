# 03. Key management

Epic label: `epic:key-management`. Tracking issue: [#97](https://github.com/EthanY33/wirefan/issues/97). Milestone: v1.2. Estimate: 36 h.

## Goal

Give API keys the controls an operator needs once more than one app shares
a server: their own rate budgets, limited permissions, an expiry date, a way
to rotate a leaked secret without changing the public key id, and live
visibility plus a way to drop abusive connections without revoking the key.

## Current state

* **Key record.** `internal/store/store.go` (lines 11-17):

  ```go
  type Key struct {
      ID         string
      Name       string
      SecretHash string
      CreatedAt  time.Time
      RevokedAt  *time.Time
  }
  ```

  The `Store` interface (lines 19-25) has `CreateKey`, `LookupKey`,
  `ListKeys`, `RevokeKey` and `Close`, and no update method.
* **Schema.** `internal/store/migrate.go` keeps an ordered, append-only
  `migrations` slice using `PRAGMA user_version`; it holds only the
  version-1 baseline. `validateMigrations` requires contiguous versions,
  each step runs in a `BEGIN IMMEDIATE` transaction, and `preflight` refuses
  a database newer than the binary. The runner is already exercised against
  multi-step lists in `migrate_test.go`.
* **Rate.** One `ratelimit.New(100, 200, time.Hour)` in
  `cmd/wirefan/main.go: run` serves every key; `internal/ratelimit.Limiter`
  has a single rps and burst.
* **Permissions.** `internal/conn/handler.go` checks channel validity,
  reserved prefixes and tokens, and nothing per key.
* **Expiry.** `internal/server/upgrade.go: ServeHTTP` and
  `internal/server/rest.go: sign` (line 176) gate only on `RevokedAt`.
* **Admin API.** `internal/server/rest.go: RegisterAdmin` (lines 73-77):
  create, list, revoke. `create` decodes with `DisallowUnknownFields`.
* **Closing conns.** `internal/hub/hub.go: CloseKey` (line 97) closes every
  conn on a key and records the key in `closedKeys`, which `Add` consults
  forever after; it is an admission barrier for revocation, so it cannot be
  used to kick connections from a key that stays valid.

## Proposed design

### Schema

Each feature appends its own migration step. Version numbers are assigned
in merge order (v2, v3, ...) because `validateMigrations` requires a
contiguous list; no feature assumes a fixed number. All new columns are
nullable, and `NULL` means "behaves as in 1.0", so every migrated key keeps
its current behavior.

| Feature | Columns |
| ------- | ------- |
| Rate overrides | `rate_rps REAL NULL`, `rate_burst INTEGER NULL` |
| Scopes | `scopes TEXT NULL` (JSON) |
| Expiry | `expires_at INTEGER NULL` (Unix ms, like the existing timestamps) |
| Rotation | `secret_hash_prev TEXT NULL`, `prev_expires_at INTEGER NULL` |

`store.Key` gains matching fields, and both the memory and SQLite stores
implement them. Rotation adds a `RotateSecret(ctx, id, newHash, prevUntil)`
method to `Store`; `internal/` is not covered by the compatibility promise.

A helper `func (k Key) Active(now time.Time) bool` (not revoked, not
expired) replaces the scattered `RevokedAt != nil` checks in `upgrade.go`,
`rest.go: sign` and the REST publish handler.

### Per-key rate overrides

`POST /v1/keys` accepts optional `rate_rps` and `rate_burst`; `GET /v1/keys`
returns them when set. `Limiter.Allow` takes the effective limits
(`Allow(key string, rps float64, burst int)`, or a resolver function set at
construction), and the conn resolves its key's limits once at upgrade from
the `Key` that `UpgradeHandler` already loads. Keys without overrides use
the server default (today's 100/200, or `--key-rate` / `--key-burst` once
the operability epic adds them). This gives keys separate budgets and makes
a starved key visible through `wirefan_rate_limited_total{scope="key"}`. It
does not stop one visitor starving a shared key on its own; that is the
per-IP sub-limit in `01-abuse-resistance.md`.

### Scopes and channel-prefix allowlists

```json
POST /v1/keys
{
  "name": "news-widget",
  "scopes": { "publish": false, "subscribe": true, "channel_prefixes": ["public-news-"] }
}
```

* `publish: false` refuses `publish` frames and `POST /v1/publish`.
* `subscribe: false` refuses `subscribe` frames, except `_wirefan-stats`
  (open question below).
* `channel_prefixes` limits both operations to channels starting with one of
  the prefixes. An empty or absent list means any channel.

The scopes are copied into `conn.Deps` from the key loaded at upgrade, so
there is no store lookup per frame. A refused frame gets a new error code:

```json
{
  "type": "error",
  "code": "FORBIDDEN_SCOPE",
  "message": "this API key may not publish",
  "op": "publish",
  "channel": "public-news-sports"
}
```

`FORBIDDEN_SCOPE` is checked right after `RESERVED_CHANNEL` in the order in
`docs/PROTOCOL.md` 7, before any rate bucket is charged.

`/v1/auth/sign` also checks `channel_prefixes`, so a key cannot mint a token
for a channel it may not use.

### Expiry

`POST /v1/keys` accepts `expires_at` (RFC 3339) or `ttl_seconds`. An expired
key is refused at `/v1/connect` (401, `wirefan_upgrade_rejected_total{reason="expired"}`,
a new label value) and at `/v1/auth/sign` (401). Because open sockets outlive
the check, a sweep goroutine started in `run` checks keys with an
`expires_at` every 30 s and closes their conns with 1008, reason
`key expired`. The sweep must not use `CloseKey`'s sticky `closedKeys` entry
for keys that are later extended, so expiry closes conns through the
non-sticky kick path below and relies on `Active()` at upgrade to refuse
new ones. The sweep goroutine is added to `internal/server/leak_test.go`.

### Secret rotation

```
POST /v1/keys/{id}/rotate?grace=24h
Authorization: Bearer <admin_token>

201 { "id": "...", "name": "...", "secret": "<new secret, shown once>", "previous_valid_until": "2026-..." }
```

The old hash moves to `secret_hash_prev` and stays valid for `sign` and
`POST /v1/publish` until `prev_expires_at`. `grace=0` drops it at once.
Verification uses the existing constant-time `auth.VerifySecret` against
both hashes. Revoking clears both. The server-wide token signing secret is
not involved, so outstanding subscribe tokens are unaffected.

### Stats and kick

```
GET  /v1/keys/{id}/stats
200 { "id": "...", "connections": 42, "distinct_ips": 7, "subscriptions": 118 }

POST /v1/keys/{id}/kick?ip=203.0.113.9
POST /v1/keys/{id}/kick?socket_id=01HKQ8...
POST /v1/keys/{id}/kick
200 { "closed": 3 }
```

`Hub.KeyStats(keyID)` and `Hub.Kick(keyID, filter, code, reason)` walk the
conn set under the read lock. `Kick` closes matching conns with 1008, reason
`kicked`, and does not touch `closedKeys`, so the key keeps working for new
connections. Kick by IP needs conns to record their source IP (plumbed by
the per-IP sub-limit issue); kick by socket id uses the socket index from
the sign-binding issue. Counters stay in process; usage is not persisted.

## Protocol and API changes

| Change | Kind |
| ------ | ---- |
| Optional `rate_rps`, `rate_burst`, `scopes`, `expires_at`, `ttl_seconds` on `POST /v1/keys`; echoed by `GET /v1/keys` | New request and response fields |
| `POST /v1/keys/{id}/rotate`, `GET /v1/keys/{id}/stats`, `POST /v1/keys/{id}/kick` (admin listener) | New paths |
| Error code `FORBIDDEN_SCOPE` | New error code |
| Close reasons `key expired` and `kicked` on code 1008 | New reasons on an existing code |
| `wirefan_upgrade_rejected_total{reason="expired"}` | New label value |
| Schema migrations appended to `internal/store/migrate.go` | Automatic upgrade |

## Compatibility impact

* `create` uses `DisallowUnknownFields`; a 1.0 server refuses the new fields
  with 400, which is the correct answer from an older server. A 1.2 server
  accepts 1.0 requests unchanged.
* Every migrated key has `NULL` in the new columns and behaves exactly as
  before. `docs/COMPATIBILITY.md` "Data on disk" is kept: upgrades are
  automatic and transactional, and an older binary refuses the migrated
  database (the existing `preflight` guard), which `deploy/deploy.sh`
  already handles by snapshotting before upgrade.
* Close code 1008 keeps its meaning (policy violation); the JS client's
  terminal-close handling in `05-client-sdks.md` treats `key expired` like
  `key revoked`, while `kicked` remains retryable.

## Task breakdown

| Order | Issue | Est. |
| ----- | ----- | ---- |
| 1 | [#60](https://github.com/EthanY33/wirefan/issues/60) | 8 h |
| 2 | [#61](https://github.com/EthanY33/wirefan/issues/61) | 10 h |
| 3 | [#63](https://github.com/EthanY33/wirefan/issues/63) | 6 h |
| 4 | [#62](https://github.com/EthanY33/wirefan/issues/62) | 6 h |
| 5 | [#64](https://github.com/EthanY33/wirefan/issues/64) | 6 h |

Dependencies: issue 3 needs the source IP on conns from
[#45](https://github.com/EthanY33/wirefan/issues/45)
and the socket index from
[#43](https://github.com/EthanY33/wirefan/issues/43).
Issue 4 uses issue 3's non-sticky close path. Issue 1 uses
`wirefan_rate_limited_total` from the operability metrics issue.

## Test plan

* Migrations: each new step passes an upgrade test from a version-1
  database and from a v0.2.0-shaped database, keys survive with `NULL` in
  the new columns, and `preflight` still refuses a newer schema.
* Rate overrides: a key created with `rate_rps=5` is limited to 5/s while a
  default key on the same server is not; a low-budget key cannot spend
  another key's tokens.
* Scopes: a subscribe-only key gets `FORBIDDEN_SCOPE` on publish (WebSocket
  and REST); a prefix-limited key cannot subscribe, publish or sign outside
  its prefixes; a key with `NULL` scopes has full access.
* Expiry: an expired key gets 401 at `/v1/connect` and `/v1/auth/sign`; an
  open socket is closed with 1008 `key expired` within one sweep interval
  (test shortens the interval); keys without expiry are unaffected; no
  goroutine leak.
* Rotation: old and new secrets both sign during the grace window and only
  the new one afterwards; `grace=0` drops the old one at once; revoke
  rejects both; the secret never appears in `GET /v1/keys` or logs.
* Stats and kick: counts match a known set of connections; kick by key, IP
  and socket id closes exactly the matching sockets with 1008 `kicked`; the
  key then accepts new connections; endpoints require the admin token.

## Open questions

* Should a subscribe-only scope still allow `_wirefan-stats`? It carries no
  client data, so allowing it is the proposed default.
* Should per-key overrides also cover the per-conn limits, or only the
  shared key bucket? Only the key bucket is proposed, to keep the conn
  limits a server-wide safety floor.
* Expiry sweep interval: 30 s is proposed. A shorter interval costs a store
  scan per tick; an index on `expires_at` keeps that cheap if key counts
  grow.
