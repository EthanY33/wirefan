# wirefan Wire Protocol — v1

## 1. Overview

wirefan is a single-binary Go WebSocket fan-out server that lets app
backends push JSON events to many browsers over named channels. This
document specifies the v1 wire protocol — the JSON message shapes, the
HTTP handshake, the auth flow, the close codes, and the limits — as
implemented today.

**Protocol identifier:** `v1`. The string `"version": "v1"` is sent in
the first server frame on every successful connection (see §5.2).

## 2. Transport

| Layer        | Choice                                                           |
| ------------ | ---------------------------------------------------------------- |
| Transport    | WebSocket (RFC 6455) over HTTP or HTTPS                          |
| WS library   | `github.com/coder/websocket` (server side)                       |
| Subprotocol  | None. Clients SHOULD NOT request a `Sec-WebSocket-Protocol`.     |
| Frame type   | `Text` only. Server writes `MessageText`; clients MUST do the same. |
| Encoding     | UTF-8 JSON. One JSON object per WebSocket frame.                 |
| Compression  | Not negotiated.                                                  |

A single WebSocket connection is bidirectional and full-duplex; client
and server frames share the same stream.

## 3. Connection establishment

### 3.1 Endpoint

```
GET /v1/connect?key=<API_KEY_ID>
Upgrade: websocket
Connection: Upgrade
```

`<API_KEY_ID>` is the `id` returned by `POST /v1/keys`. The secret is
**not** sent on this endpoint; secrets are only used by app servers
calling `/v1/auth/sign`.

### 3.2 Pre-upgrade rejection codes

Before the WebSocket upgrade is accepted the server may reject the
request with a plain HTTP response:

| HTTP | Reason                                                           |
| ---- | ---------------------------------------------------------------- |
| 401  | `key` query param missing, unknown, or revoked.                  |
| 403  | (Reserved.) Origin not in allowlist — surfaced by the WS lib as upgrade failure. |
| 429  | Per-source-IP active-connection cap reached (default 200, `WIREFAN_IP_CAP`). |

### 3.3 First server frame

Immediately after the upgrade the server sends a `connected` frame
(see §5.2). The client MUST treat the connection as not-yet-ready
until it has received this frame.

## 4. Auth flow

`public-*` (and any non-`private-`, non-`presence-`,
non-`_`-prefixed) channel names require no token. `private-*` and
`presence-*` channels require an HMAC token that is **bound to the
issuing socket_id**, so a token leaked to a third party cannot be
replayed on a different connection.

```
+----------+        +-------------+       +---------+         +----------+
| Operator |        | App server  |       | Browser |         | wirefan  |
+----------+        +-------------+       +---------+         +----------+
     |  POST /v1/keys (Bearer admin)      |                       |
     |---------------------------------------------------->       |
     |  { id, secret }                    |                       |
     |<----------------------------------------------------       |
     |  ship `id` to browser, keep `secret` server-side           |
     |                                    |                       |
                                          |   GET /v1/connect?key=<id>
                                          |---------------------->|
                                          |  { type:connected,    |
                                          |    socket_id, version}|
                                          |<----------------------|
                                          |                       |
                                          |  POST /v1/auth/sign   |
                                          |  Bearer <id>:<secret> |
                                          |  { socket_id, channel}|
                                          |   (browser asks app server) <-+
                                          |                       |       |
     (app server signs token using  channel + socket_id)          |       |
                                          |   { token }           |       |
                                          |---------------------->|       |
                                          |  { type:subscribe,    |       |
                                          |    channel:"private-x"|       |
                                          |    token: "<sig>" }   |       |
                                          |---------------------->|       |
                                          |  { type:subscribed,   |       |
                                          |    channel }          |       |
                                          |<----------------------|       |
```

Token TTL is **5 minutes** from issuance (`auth.SignToken`, called by
the sign handler in `internal/server/rest.go`). Tokens are also
**single-use**: each carries a random `jti` that the server records on
first successful verify, so replaying the same token (even on the same
connection) fails with `AUTH_REPLAYED`. Reconnects get a new
`socket_id` and therefore must obtain a new token.

## 5. Message shapes

All frames are JSON objects with a required `type` field. Fields not
listed below MUST be ignored by both sides (forward-compatibility).

### 5.1 Direction summary

| Type          | Direction       | Closes conn on failure? |
| ------------- | --------------- | ----------------------- |
| `connected`   | Server → Client | n/a                     |
| `subscribed`  | Server → Client | n/a                     |
| `unsubscribed`| Server → Client | n/a                     |
| `event`       | Server → Client | n/a                     |
| `error`       | Server → Client | No                      |
| `subscribe`   | Client → Server | No                      |
| `unsubscribe` | Client → Server | No                      |
| `publish`     | Client → Server | No                      |

### 5.2 `connected` (Server → Client)

Sent once, immediately after upgrade.

```json
{
  "type": "connected",
  "socket_id": "01HKQ8M5YVF3T6X9N2Q1ZRWBP4",
  "version": "v1"
}
```

| Field       | Type   | Notes                                            |
| ----------- | ------ | ------------------------------------------------ |
| `socket_id` | string | ULID (Crockford base32, 26 chars). Per-connection. |
| `version`   | string | Protocol version. Currently `"v1"`.              |

### 5.3 `subscribe` (Client → Server)

```json
{
  "type": "subscribe",
  "channel": "private-room42",
  "token": "1714932000000:9f8a...c41d:VXNl...c2lnbg"
}
```

| Field     | Type   | Required when                       |
| --------- | ------ | ----------------------------------- |
| `channel` | string | always                              |
| `token`   | string | only when `channel` starts with `private-` |

### 5.4 `subscribed` (Server → Client)

Acknowledges an accepted subscribe (idempotent — re-subscribing to a
channel you are already on returns success without duplicate state).

```json
{ "type": "subscribed", "channel": "private-room42" }
```

### 5.5 `unsubscribe` (Client → Server)

```json
{ "type": "unsubscribe", "channel": "private-room42" }
```

### 5.6 `unsubscribed` (Server → Client)

Acknowledges an unsubscribe. Always emitted, even if the client was
not subscribed (idempotent).

```json
{ "type": "unsubscribed", "channel": "private-room42" }
```

### 5.7 `publish` (Client → Server)

```json
{
  "type": "publish",
  "channel": "private-room42",
  "data": { "msg": "hello", "from": "alice" }
}
```

| Field     | Type      | Notes                                        |
| --------- | --------- | -------------------------------------------- |
| `channel` | string    | The publisher MUST be subscribed first.      |
| `data`    | any JSON  | Echoed verbatim to subscribers as `event.data`. |

The publisher receives the resulting `event` only if it is itself a
subscriber (no client-side echo suppression — the protocol does not
distinguish self-publishes).

### 5.8 `event` (Server → Client)

Delivered to every subscriber of `channel`.

```json
{
  "type": "event",
  "channel": "private-room42",
  "data": { "msg": "hello", "from": "alice" },
  "id": "01HKQ8M9F7XPQVJZ4YCZ7G3W2A"
}
```

| Field     | Type      | Notes                                        |
| --------- | --------- | -------------------------------------------- |
| `id`      | string    | Server-assigned ULID (per `event`).          |
| `data`    | any JSON  | Verbatim copy of the publisher's `data`.     |

### 5.9 `error` (Server → Client)

```json
{ "type": "error", "code": "AUTH_FAILED", "message": "invalid token" }
```

Codes are listed in §7. The connection is **not** closed; the client
may retry or take corrective action.

### 5.10 Token format

`SignToken` in `internal/auth/token.go` produces a three-part value:

```
<expiry_unix_ms>:<jti>:<base64url_no_padding(HMAC_SHA256(secret, "<expiry_unix_ms>|<socket_id>|<channel>|<jti>"))>
```

`<jti>` is 16 random bytes, hex-encoded (32 chars), generated fresh
per token. It is part of the MAC payload, so it cannot be swapped
without invalidating the signature.

Verification (`auth.VerifyTokenAgainst`) proceeds in order: parse the
three parts, reject if expired, recompute the MAC for
`(expiry, socket_id, channel, jti)` and compare with `hmac.Equal`,
then check the `jti` against the server's replay cache. A `jti` seen
before is rejected with `AUTH_REPLAYED`, which makes every token
one-time-use; every other failure mode (malformed, expired, bad
signature) surfaces as `AUTH_FAILED`. Expired cache entries are
swept once a minute, bounding replay-cache memory to roughly the
token issuance rate times the 5-minute TTL.

## 6. Channel naming

| Prefix          | Token required | Publish allowed by clients | Notes                                     |
| --------------- | -------------- | -------------------------- | ----------------------------------------- |
| `public-*`      | No             | Yes                        | Any subscriber can also publish.          |
| (any other)     | No             | Yes                        | Treated like `public-` for protocol purposes. |
| `private-*`     | Yes (HMAC)     | Yes                        | Subscribe requires a `token` bound to `socket_id`. |
| `presence-*`    | Yes (HMAC)     | Yes                        | Auth like `private-`; member-list events are not implemented. |
| `_wirefan-stats` | No | No (publish returns `RESERVED_CHANNEL`) | Read-only carve-out: clients may subscribe to receive server stats snapshots. |
| `_*` (any other underscore name) | n/a (client subscribe and publish both return `RESERVED_CHANNEL`) | No | Reserved for the server. |

Channel names are limited to 128 bytes, must be non-empty, and may
not contain control characters; violations return `BAD_CHANNEL`.

The reserved name in use today is `_wirefan-stats`, periodically
populated by `hub.PublishStatsLoop` with a server-generated `event`
frame carrying aggregate counters (`connections`, `channels`,
`published`, `dropped`). It is the single exception to the `_*`
reservation: clients may subscribe to exactly `_wirefan-stats`
(the demo's live stats panel depends on this), but publishing to it,
and subscribing to any other `_`-prefixed name, still returns
`RESERVED_CHANNEL`.

## 7. Error codes

All emitted as `{"type":"error", "code":"…", "message":"…"}`. None
close the WebSocket — the client can keep using the connection.

| Code                | Trigger                                                              |
| ------------------- | -------------------------------------------------------------------- |
| `BAD_JSON`          | Frame body could not be unmarshaled into the incoming envelope.      |
| `BAD_CHANNEL`       | Channel name empty, over 128 bytes, or contains control characters.  |
| `BAD_TYPE`          | `type` is not one of `subscribe`, `unsubscribe`, `publish`.          |
| `AUTH_FAILED`       | `private-*`/`presence-*` subscribe with missing, malformed, expired, or invalid token. |
| `AUTH_REPLAYED`     | Subscribe token whose `jti` was already used (tokens are single-use). |
| `NOT_SUBSCRIBED`    | `publish` to a channel this conn has not subscribed to.              |
| `RATE_LIMITED`      | Per-API-key budget exceeded: publish rate, or control-op (subscribe/unsubscribe) rate. |
| `RATE_LIMITED_CONN` | Per-connection publish budget exceeded (see §9).                     |
| `RESERVED_CHANNEL`  | Client publish targeted a `_`-prefixed channel, or subscribe targeted one other than `_wirefan-stats`. |
| `LIMIT_CHANNELS`    | Conn already has 64 active subscriptions.                            |
| `LIMIT_SUBSCRIBERS` | Channel already has 10 000 subscribers.                              |
| `SUBSCRIBE_FAILED`  | Subscribe kept racing the registry sweeper past its internal retry budget; safe for the client to retry. |

## 8. WebSocket close codes

The server may close the connection with one of:

| Code | Constant            | Cause                                                          |
| ---- | ------------------- | -------------------------------------------------------------- |
| 1000 | `NormalClosure`     | Normal close from the server's writePump path.                 |
| 1001 | `GoingAway`         | Read deadline exceeded (60 s), or `Hub.Drain` shutdown sweep.  |
| 1008 | `PolicyViolation`   | Slow-consumer disconnect under `PolicyDisconnect` (see §11).   |
| 1009 | `MessageTooBig`     | Inbound frame exceeded the 64 KiB read limit (set by the WS lib). |

The current implementation does **not** emit 1003 (`UnsupportedData`)
on bad JSON; bad JSON returns an `error` frame and keeps the
connection open. A v2 protocol may tighten this.

## 9. Limits

| Limit                            | Value          | Source                                           |
| -------------------------------- | -------------- | ------------------------------------------------ |
| Max inbound frame size           | 64 KiB         | `internal/conn/pumps.go` (`SetReadLimit`)        |
| Max channel name length          | 128 bytes      | `maxChannelNameLen` in `handler.go`              |
| Max channels per connection      | 64             | `defaultMaxChannelsPerConn` in `conn.go`         |
| Max subscribers per channel      | 10 000         | `defaultMaxSubsPerChannel` in `conn.go`          |
| Publish rate per API key, sustained | 100 / sec   | `ratelimit.New(100, 200, …)` in `cmd/wirefan/main.go` |
| Publish rate per API key, burst  | 200            | same                                             |
| Publish rate per connection, sustained / burst | 50 / sec, 100 | `defaultConnPublishRate` / `defaultConnPublishBurst` in `conn.go` |
| Active conns per source IP       | 200 (override: `WIREFAN_IP_CAP` env) | `defaultIPCap` in `internal/server/upgrade.go` |
| Per-conn send buffer             | 64 messages    | `sendChanSize` in `conn.go`                      |
| Token TTL                        | 5 min          | `internal/server/rest.go`                        |

The per-API-key bucket is shared by every connection authenticated
with the same key (subscribe/unsubscribe control ops draw from it
too); the per-connection bucket bounds any single socket on top of
that.

## 10. Ordering guarantees

* **Single publisher → single subscriber, single channel:** FIFO. The
  `Fanout` implementations preserve publish order and the per-conn
  send chan is also FIFO.
* **Cross-publisher:** No global ordering. Two publishers writing
  concurrently to the same channel interleave arbitrarily, and
  different subscribers may observe their messages in different
  orders: nothing serialises concurrent `Broadcast` calls on one
  channel.
* **Cross-channel:** No ordering. Two channels share no synchronisation.
* **At-most-once delivery.** Drops under backpressure (see §11) are
  silent from the subscriber's point of view; no resend or sequence
  number is provided.

`event.id` is a ULID and is monotonically increasing within a single
publisher's wall-clock tick, but it MUST NOT be relied on for
cross-publisher ordering.

## 11. Backpressure policies

When a subscriber's per-conn send buffer (capacity 64) is full, the
configured `Policy` decides what happens (`internal/conn/policy.go`):

| Policy             | Behaviour when buffer is full                                    |
| ------------------ | ---------------------------------------------------------------- |
| `PolicyDisconnect` | Returns `ErrSlowConsumer`; the conn is closed with **1008** (PolicyViolation). **Default.** |
| `PolicyDropOldest` | Evicts the head message and enqueues the new one. Never errors. |
| `PolicyDropNewest` | Drops the new message. Never errors.                             |

The policy is server-wide today (set in `cmd/wirefan/main.go`). A
per-conn override flag is specified but **not yet wired** — see Task
15 follow-ups. Drops increment `wirefan_messages_dropped_total{reason="slow_consumer"}`.

No `error` frame is sent on the disconnect path: the client sees a
1008 close. Drop-oldest / drop-newest are silent by design.

## 12. Heartbeat

* The server sends a WebSocket `Ping` every **30 s** (`pingInterval`
  in `conn.go`).
* The server's read deadline per frame is **60 s** (`readDeadline`).
  A missed pong / silent peer therefore closes the conn with **1001
  GoingAway** within ~60 s.
* `Pong` handling is internal to `coder/websocket`; clients do not
  send or interpret heartbeat frames as JSON. Browsers running the
  W3C WebSocket API reply to pings automatically.
* Write timeout is **10 s** (`writeDeadline`); a stalled write closes
  the conn from the writePump path.

## 13. REST control plane

Endpoints are split across two listeners. The **public listener**
(`--listen`, default `:8080`) carries the data plane and the one
endpoint app servers call; the **admin listener** (`--admin-addr`,
default `127.0.0.1:6060`, loopback on purpose) carries key management,
metrics, and profiling.

Public listener:

| Method | Path                  | Auth                          | Purpose                                |
| ------ | --------------------- | ----------------------------- | -------------------------------------- |
| GET    | `/v1/connect`         | `?key=<id>`                   | WebSocket upgrade (§3).                |
| POST   | `/v1/auth/sign`       | `Bearer <id>:<secret>`        | Sign a private-channel token. Body: `{socket_id, channel}`. Returns `{token}`. |
| GET    | `/v1/health`          | none                          | `200 ok` while serving; `503 draining` during shutdown. |
| GET    | `/`                   | none                          | Embedded demo client (web/).           |

Admin listener:

| Method | Path                  | Auth                          | Purpose                                |
| ------ | --------------------- | ----------------------------- | -------------------------------------- |
| POST   | `/v1/keys`            | `Bearer <admin_token>`        | Create API key. Returns `{id, name, secret}` (secret shown once). |
| GET    | `/v1/keys`            | `Bearer <admin_token>`        | List keys (no secrets).                |
| DELETE | `/v1/keys/{id}`       | `Bearer <admin_token>`        | Revoke a key.                          |
| GET    | `/metrics`            | none (loopback-bound)         | Prometheus exposition.                 |
| ANY    | `/debug/pprof/*`      | none (loopback-bound)         | Standard `net/http/pprof` endpoints.   |

The admin token is **persisted, not printed**. Resolution order at
boot: the `WIREFAN_ADMIN_TOKEN` env var if set; else the contents of
`<state-dir>/admin.token` (`WIREFAN_STATE_DIR`, default `./var`); else
a freshly generated token written to that file with mode 0600 and
reused on every subsequent boot. Operators retrieve it by reading the
file.

## 14. Versioning

The current protocol identifier is **`v1`**, broadcast in every
`connected` frame's `version` field. Clients SHOULD assert this value
on connect.

Compatibility rules for v1:

* Servers MUST accept and ignore unknown JSON fields on inbound
  frames.
* Clients MUST accept and ignore unknown JSON fields on outbound
  frames.
* New optional fields can be added in v1 minor revisions without a
  protocol bump.

A breaking change (renamed type, removed field, new required field)
will introduce **`v2`** under a new endpoint path
(`/v2/connect`). The v1 endpoint will continue to honour the v1
contract for at least one minor release after v2 ships. The exact
deprecation policy will be published alongside the v2 spec.
