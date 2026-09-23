# wirefan wire protocol v1

## 1. Overview

wirefan is a single-binary Go WebSocket fan-out server. Clients subscribe
to named channels over WebSocket, and a JSON event published to a channel
is delivered to every subscriber on it. This document specifies the v1
wire protocol as wirefan implements it: the JSON message shapes, the HTTP
handshake, the auth flow, the error and close codes, and the limits.
[`COMPATIBILITY.md`](COMPATIBILITY.md) says which parts of it the 1.x
releases promise to keep.

**Protocol identifier:** `v1`. The string `"version": "v1"` is sent in
the first server frame on every successful connection (see §5.2).

## 2. Transport

| Layer        | Choice                                                           |
| ------------ | ---------------------------------------------------------------- |
| Transport    | WebSocket (RFC 6455). wirefan serves plain HTTP; `wss://` comes from a TLS-terminating proxy in front of it (see [`DEPLOY.md`](DEPLOY.md)). |
| WS library   | `github.com/coder/websocket` (server side)                       |
| Subprotocol  | None. Clients SHOULD NOT request a `Sec-WebSocket-Protocol`.     |
| Message type | Text only. The server sends text messages; clients MUST do the same. |
| Encoding     | UTF-8 JSON. One JSON object per WebSocket message.               |
| Compression  | Not negotiated.                                                  |

A single WebSocket connection is bidirectional and full-duplex; client
and server frames share the same stream. In this document a protocol
frame, such as a `subscribe` frame, is one JSON object sent as one
WebSocket message; close, ping and pong frames are WebSocket control
frames.

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

Before accepting the upgrade the server runs these checks in order and
answers the first one that fails with a plain-text HTTP response:

| Order | HTTP          | Reason                                                   |
| ----- | ------------- | -------------------------------------------------------- |
| 0     | 503           | The server is shutting down (`draining`). Reconnect with backoff; another instance or the restarted process will accept. |
| 1     | 401           | `key` query param missing (`missing key`), or the key is unknown or revoked (`invalid key`). |
| 2     | 429           | Per-source-IP active-connection cap reached (default 200, `WIREFAN_IP_CAP`). |
| 3     | 426, 405, 400 | WebSocket handshake rejected by `coder/websocket`: 426 when the request is older than HTTP/1.1 or its `Connection` and `Upgrade` headers do not ask for a WebSocket upgrade; 405 when the method is not `GET`; 400 when `Sec-WebSocket-Version` is not `13` or `Sec-WebSocket-Key` is missing, repeated or malformed. |
| 4     | 403           | `Origin` not allowed by `--allowed-origins`. A request without an `Origin` header, or whose `Origin` host equals its `Host`, passes this check. |

Because the key is checked first, a plain GET without upgrade headers
gets 401 rather than 426 when its key is missing or invalid.

For the 429 check the source IP is the TCP peer address or, when the peer
is listed in `WIREFAN_TRUSTED_PROXIES`, the rightmost `X-Forwarded-For`
hop not in that list. IPv4 clients are counted per address and IPv6
clients per /64 prefix; an IPv4-mapped IPv6 address counts as its IPv4
address.

### 3.3 First server frame

Immediately after the upgrade the server sends a `connected` frame
(see §5.2). The client MUST treat the connection as not-yet-ready
until it has received this frame. The one exception: if the API key is
revoked while the upgrade is in progress, the server closes the
connection with 1008 `key revoked` instead of sending `connected` (§8).
Likewise, a connection whose upgrade completes just as shutdown begins is
closed with 1001 `shutdown` before `connected`.

## 4. Auth flow

`public-*` (and any non-`private-`, non-`presence-`,
non-`_`-prefixed) channel names require no token. `private-*` and
`presence-*` channels require an HMAC token that is **bound to the
issuing socket_id** and to one channel, so a token leaked to a third
party cannot be used on a different connection or for a different
channel.

wirefan signs every token itself, with a signing secret it generates at
startup and holds only in memory. The app server never sees that secret.
It holds an API key secret, which it uses to ask wirefan for a token after
running its own access check. Before any of this, the operator creates the
API key with `POST /v1/keys` (§13), gives the key `id` to the browser app
and keeps the key `secret` in the app server's configuration.

```
 Browser                 App server                  wirefan
    |                         |                         |
    | GET /v1/connect?key=<id>|                         |
    |-------------------------------------------------->|
    |                         | connected, socket_id S  |
    |<--------------------------------------------------|
    | may I join private-x?   |                         |
    | (socket_id S)           |                         |
    |------------------------>|                         |
    |                         | your own login check    |
    |                         |                         |
    |                         | POST /v1/auth/sign      |
    |                         | Bearer <id>:<secret>    |
    |                         | {socket_id S, channel}  |
    |                         |------------------------>|
    |                         |                         | signs T with its
    |                         |                         | per-boot secret
    |                         | 200 {"token": T}        |
    |                         |<------------------------|
    | token T                 |                         |
    |<------------------------|                         |
    | subscribe, channel private-x, token T             |
    |-------------------------------------------------->|
    |                         | subscribed, private-x   |
    |<--------------------------------------------------|
```

`POST /v1/auth/sign` is on the public listener. It takes
`Authorization: Bearer <id>:<secret>`; the `Bearer ` prefix (exactly that
case and one space) is optional, and a bare `<id>:<secret>` is accepted
too. The body is `{"socket_id": "...", "channel": "..."}`. The handler
answers:

| HTTP | Body                                            | When                                   |
| ---- | ----------------------------------------------- | -------------------------------------- |
| 401  | `bad credentials`                               | The credentials are missing or contain no `:`, the key is unknown or revoked, or the secret is wrong. Checked before the body is read. |
| 400  | `bad request`                                   | The body does not decode as that JSON object. |
| 400  | `bad request: socket_id is not a valid socket id` | `socket_id` is not a ULID (26 Crockford base32 characters), the only shape `/v1/connect` issues. |
| 400  | `bad request: channel must be a valid private- or presence- channel name` | `channel` is empty, over 128 bytes, contains a C0 control character or DEL, or does not start with `private-` or `presence-`. Public channels and `_wirefan-stats` need no token, so none is minted for them. |
| 200  | `{"token": "..."}`                              | Otherwise.                             |

The server does not check that `socket_id` belongs to an open connection,
or to one opened with the same API key: the token is bound only to the
`socket_id`, the channel and its expiry.

Token TTL is **5 minutes** from issuance (`auth.SignToken`, called by
the sign handler in `internal/server/rest.go`). Tokens are also
**single-use**: each carries a random `jti` that the server records on
first successful verify. A token is spent as soon as it verifies, even if
the subscribe then fails with `LIMIT_CHANNELS`, `LIMIT_SUBSCRIBERS` or
`SUBSCRIBE_FAILED`; a retry needs a fresh token. Presenting a spent,
unexpired token again on the same connection for the same channel, after
that subscribe failed or after the channel was unsubscribed, gets
`AUTH_REPLAYED`. On any other connection it fails the `socket_id` binding
and gets `AUTH_FAILED`, because the signature is checked before the
replay cache (§5.10). Re-subscribing to a channel the connection still
holds is acked without looking at the token (§5.4). Reconnects get a new
`socket_id` and therefore must obtain a new token. A server restart
generates a new signing secret, so every token issued before it stops
verifying.

## 5. Message shapes

All frames are JSON objects with a required `type` field. Fields not
listed below MUST be ignored by both sides (forward-compatibility). The
order of fields within an object is not significant.

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
| `token`   | string | only when `channel` starts with `private-` or `presence-` and the connection does not already hold it; ignored otherwise |

### 5.4 `subscribed` (Server → Client)

Acknowledges an accepted subscribe.

```json
{ "type": "subscribed", "channel": "private-room42" }
```

Subscribing to a channel the connection already holds returns
`subscribed` again without duplicate state, and without requiring,
verifying or consuming a token. The repeated subscribe still goes through
the channel-name, reserved-name and rate-limit checks (§7, §9).

### 5.5 `unsubscribe` (Client → Server)

```json
{ "type": "unsubscribe", "channel": "private-room42" }
```

### 5.6 `unsubscribed` (Server → Client)

Acknowledges an unsubscribe, whether or not the client was subscribed
(idempotent). An unsubscribe refused with `BAD_CHANNEL`,
`RATE_LIMITED_CONN` or `RATE_LIMITED` gets that `error` frame instead.

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
| `channel` | string    | The publisher MUST be subscribed first (`NOT_SUBSCRIBED` otherwise). |
| `data`    | any JSON  | Delivered to subscribers as `event.data` (§5.8). If absent, subscribers receive `null`. |

A successful publish is not acknowledged; a refused one gets an `error`
frame with `"op": "publish"`. Because a publisher must be subscribed to
the channel, it receives its own `event` like every other subscriber (the
server does no echo suppression).

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
| `id`      | string    | Server-assigned ULID, one per `event`. Events on `_wirefan-stats` carry a ULID too. |
| `data`    | any JSON  | The publisher's `data` as the same JSON value, re-encoded compactly (see below). |

The server re-encodes `data` before fanning it out: whitespace between
JSON tokens is removed and everything else, including escape sequences
inside strings, passes through unchanged. `<`, `>` and `&` are not
rewritten as `\u00XX` escapes, so an event is about as large as the
publish that produced it. An absent `data` field is sent as `null`.

### 5.9 `error` (Server → Client)

```json
{
  "type": "error",
  "code": "AUTH_FAILED",
  "message": "invalid token",
  "op": "subscribe",
  "channel": "private-room42"
}
```

| Field     | Type   | Notes                                              |
| --------- | ------ | -------------------------------------------------- |
| `code`    | string | Machine-readable code, listed in §7.               |
| `message` | string | Human-readable text. Match on `code`, not on this. |
| `op`      | string | Optional. `"subscribe"`, `"unsubscribe"` or `"publish"` when the error answers a client frame of that type. Every error answering such a frame carries it; `BAD_JSON` and `BAD_TYPE` do not. |
| `channel` | string | Optional. The channel exactly as the client sent it in that frame. Omitted when the frame had no (or an empty) channel, and when the channel is longer than 128 bytes, since such a name cannot be a real channel. `op` is still present in both cases. |

`op` and `channel` let a client match an error to the request that caused
it. The connection is **not** closed; the client may retry or take
corrective action.

### 5.10 Token format

Clients MUST treat the subscribe token as an opaque string. Only wirefan
produces and reads it, and its layout may change in any release. What
follows describes the current layout for reference.

`SignToken` in `internal/auth/token.go` produces a three-part value:

```
<expiry_unix_ms>:<jti>:<base64url_no_padding(mac)>

mac = HMAC_SHA256(signing_secret,
        "v1|<expiry_unix_ms>|<len(socket_id)>:<socket_id>|<len(channel)>:<channel>|<jti>")
```

`len(...)` is the field's length in bytes, in decimal. The length
prefixes keep bytes from moving from one field into its neighbour, so a
MAC for one `(socket_id, channel)` pair cannot be reshaped into a valid
MAC for another. `signing_secret` is the per-boot secret from §4. `<jti>`
is 16 random bytes, hex-encoded (32 lowercase chars), generated fresh per
token. It is part of the MAC payload, so it cannot be swapped without
invalidating the signature.

Verification (`auth.VerifyTokenAgainst`) proceeds in order: split the
token into its three parts, parse the expiry, reject if expired, require
the `jti` to be exactly 32 lowercase hex characters, decode the MAC,
recompute it for `(expiry, socket_id, channel, jti)` and compare with
`hmac.Equal`, then check the `jti` against the server's replay cache. A
`jti` seen before is rejected with `AUTH_REPLAYED`, which makes every
token one-time-use; every other failure mode (missing, malformed,
expired, bad signature) surfaces as `AUTH_FAILED`. The replay cache is
swept once a minute and drops entries whose token has expired, so it
only holds tokens verified within roughly the last six minutes.

## 6. Channel naming

| Prefix          | Token required | Publish allowed by clients | Notes                                     |
| --------------- | -------------- | -------------------------- | ----------------------------------------- |
| `public-*`      | No             | Yes                        | Any subscriber can also publish.          |
| (any other)     | No             | Yes                        | Treated like `public-` for protocol purposes. |
| `private-*`     | Yes (HMAC)     | Yes                        | Subscribe requires a `token` bound to `socket_id`. |
| `presence-*`    | Yes (HMAC)     | Yes                        | Auth like `private-`; member-list events are not implemented. |
| `_wirefan-stats` | No | No (publish returns `RESERVED_CHANNEL`) | Read-only carve-out: clients may subscribe to receive server stats snapshots. |
| `_*` (any other underscore name) | n/a (client subscribe and publish both return `RESERVED_CHANNEL`) | No | Reserved for the server. |

Prefix matching is case-sensitive: `Private-x` is an ordinary public
channel. Channel names are server-wide, not scoped to an API key: every
connection that subscribes to a name joins the same channel, whichever
key it connected with.

Channel names must be non-empty, at most 128 bytes, and may not contain
C0 control characters (U+0000 to U+001F) or DEL (U+007F); violations
return `BAD_CHANNEL` on subscribe, unsubscribe and publish. Other
characters are accepted, including the C1 control characters U+0080 to
U+009F.

The reserved name in use today is `_wirefan-stats`, populated every 5
seconds by `hub.PublishStatsLoop` with a server-generated `event` frame
whose `data` holds integer counters: `connections`, `channels` (this
count includes `_wirefan-stats` itself), `published`,
`messages_published_total` (the same value as `published`) and `dropped`.
It is the single exception to the `_*` reservation: clients may subscribe
to exactly `_wirefan-stats` (the demo's live stats panel depends on
this), but publishing to it, and subscribing to any other `_`-prefixed
name, still returns `RESERVED_CHANNEL`.

## 7. Error codes

All emitted as `error` frames (§5.9). None close the WebSocket: the
client can keep using the connection. A client MUST treat a code it does
not recognize as a failure of the operation it answers (§14).

| Code                | `op`                      | Trigger                                                  |
| ------------------- | ------------------------- | -------------------------------------------------------- |
| `BAD_JSON`          | none                      | Frame body is not valid UTF-8 (checked first, so invalid bytes are never relayed to other clients), or could not be unmarshaled into the incoming envelope (for example invalid JSON, a JSON array, or a known field with the wrong JSON type). |
| `BAD_CHANNEL`       | subscribe, unsubscribe, publish | Channel name empty, over 128 bytes, or contains a C0 control character or DEL. |
| `BAD_TYPE`          | none                      | `type` is missing or not one of `subscribe`, `unsubscribe`, `publish`. |
| `AUTH_FAILED`       | subscribe                 | `private-*`/`presence-*` subscribe with missing, malformed, expired, or invalid token. |
| `AUTH_REPLAYED`     | subscribe                 | Token that is valid for this connection and channel but whose `jti` was already used (tokens are single-use, §4). |
| `NOT_SUBSCRIBED`    | publish                   | `publish` to a channel this conn has not subscribed to.  |
| `RATE_LIMITED`      | subscribe, unsubscribe, publish | Per-API-key budget exceeded. Publish, subscribe and unsubscribe all draw from it (see §9). |
| `RATE_LIMITED_CONN` | subscribe, unsubscribe, publish | Per-connection budget exceeded: the publish budget for `publish`, the control budget for `subscribe` and `unsubscribe` (see §9). |
| `RESERVED_CHANNEL`  | subscribe, publish        | Client publish targeted a `_`-prefixed channel, or subscribe targeted one other than `_wirefan-stats`. |
| `LIMIT_CHANNELS`    | subscribe                 | Conn already has 64 active subscriptions.                |
| `LIMIT_SUBSCRIBERS` | subscribe                 | Channel already has 10 000 subscribers.                  |
| `SUBSCRIBE_FAILED`  | subscribe                 | Subscribe lost the race with the registry sweeper on all three of its attempts; safe for the client to retry (with a fresh token on `private-`/`presence-` channels, see §4). |

The server checks each frame in a fixed order and reports only the first
failure:

* `subscribe`: `BAD_CHANNEL`, `RESERVED_CHANNEL`, `RATE_LIMITED_CONN`,
  `RATE_LIMITED`; then, if the connection already holds the channel, a
  `subscribed` ack; otherwise `AUTH_FAILED` / `AUTH_REPLAYED`,
  `LIMIT_CHANNELS`, then `LIMIT_SUBSCRIBERS` / `SUBSCRIBE_FAILED`.
* `unsubscribe`: `BAD_CHANNEL`, `RATE_LIMITED_CONN`, `RATE_LIMITED`.
* `publish`: `BAD_CHANNEL`, `RESERVED_CHANNEL`, `NOT_SUBSCRIBED`,
  `RATE_LIMITED_CONN`, `RATE_LIMITED`.

## 8. WebSocket close codes

The server closes connections with these codes:

| Code | Constant            | Reason           | Cause                                         |
| ---- | ------------------- | ---------------- | --------------------------------------------- |
| 1001 | `GoingAway`         | `shutdown`       | Graceful shutdown: the `Hub.Drain` sweep closes every open connection, and a connection whose upgrade completes after the sweep started is closed the same way before `connected`. Connections still open when the 30 s drain window ends are force-closed at the TCP level. |
| 1002 | `ProtocolError`     | set by the WS library | `coder/websocket` detected a WebSocket protocol violation (for example reserved bits set, an unknown opcode or a malformed control frame). |
| 1008 | `PolicyViolation`   | `slow consumer`  | Slow-consumer disconnect under `PolicyDisconnect` (see §11). |
| 1008 | `PolicyViolation`   | `key revoked`    | The connection's API key was revoked with `DELETE /v1/keys/{id}` (§13). Also sent, before any `connected` frame, to a connection whose upgrade overlapped the revoke. |
| 1009 | `MessageTooBig`     | set by the WS library | Inbound message exceeded the 64 KiB read limit (§9). |
| 1011 | `InternalError`     | `send chan full at start` | The `connected` frame could not be queued at connection start. The send buffer is new and empty at that point, so this is not expected in practice. |

Delivery of these close frames is best-effort. After a 1002 or 1009 the
server closes the TCP connection at once, and unread data from the peer
can turn that into a TCP reset that discards the close frame. A 1008 or
1001 close handshake that the peer does not complete is cut off by
closing the TCP connection.

When the client starts the close handshake, the server's reply echoes
the client's own code and reason. That echo is the only way a 1000
(`NormalClosure`) comes from the server.

A client observes 1006 (abnormal closure, never sent on the wire) when
the connection ends without a close frame. The server ends connections
that way when a peer fails the ping/pong liveness check (§12) and when a
write does not complete within 10 s (§12). A connection force-closed at
the end of the shutdown drain also shows 1006 if no close frame reached
it first.

Bad JSON is answered with a `BAD_JSON` error frame and the connection
stays open; the server never sends 1003 (`UnsupportedData`).

## 9. Limits

| Limit                            | Value          | Source                                           |
| -------------------------------- | -------------- | ------------------------------------------------ |
| Max inbound message size         | 64 KiB         | `internal/conn/pumps.go` (`SetReadLimit`)        |
| Max channel name length          | 128 bytes      | `maxChannelNameLen` in `handler.go`              |
| Max channels per connection      | 64             | `defaultMaxChannelsPerConn` in `conn.go`         |
| Max subscribers per channel      | 10 000         | `defaultMaxSubsPerChannel` in `conn.go`          |
| Rate per API key (publish, subscribe and unsubscribe combined), sustained / burst | 100 / sec, 200 | `ratelimit.New(100, 200, …)` in `cmd/wirefan/main.go` |
| Publish rate per connection, sustained / burst | 50 / sec, 100 | `defaultConnPublishRate` / `defaultConnPublishBurst` in `conn.go` |
| Subscribe + unsubscribe rate per connection, sustained / burst | 20 / sec, 64 | `defaultConnControlRate` / `defaultConnControlBurst` in `conn.go` |
| Active conns per source IP       | 200 (override: `WIREFAN_IP_CAP` env); IPv6 counted per /64 | `defaultIPCap` and `ipCapKey` in `internal/server/upgrade.go` |
| Per-conn send buffer             | 64 messages    | `sendChanSize` in `conn.go`                      |
| Token TTL                        | 5 min          | `internal/server/rest.go`                        |

Every publish, subscribe and unsubscribe is charged first to the
connection's own bucket for its kind (publish, or control for subscribe
and unsubscribe), then to the per-API-key bucket, which is shared by
every connection opened with the same key. A request refused by a
per-connection bucket spends no per-key budget. A frame refused by an
earlier check in §7 (for example `BAD_CHANNEL`, `RESERVED_CHANNEL` or
`NOT_SUBSCRIBED`) is not charged at all.

## 10. Ordering guarantees

* **Single publisher → single subscriber, single channel:** FIFO. Each
  subscriber's send buffer is FIFO, and both `Fanout` implementations
  preserve publish order within a channel. With `--fanout=sharded` that
  holds because every channel maps to one worker queue.
* **Cross-publisher:** No global ordering. Two publishers writing
  concurrently to the same channel interleave arbitrarily. Under the
  default `--fanout=per-conn`, different subscribers may observe their
  messages in different orders, because nothing serialises concurrent
  `Broadcast` calls on one channel. Under `--fanout=sharded` one worker
  handles each channel, so every subscriber sees the same interleaving.
  Clients MUST NOT rely on either behaviour.
* **Cross-channel:** No ordering. Under `--fanout=sharded`, one
  publisher's events on two different channels may go through different
  worker queues and can then reach a subscriber of both out of order.
* **At-most-once delivery.** A subscriber that cannot keep up is
  disconnected (see §11), and events published while a client is
  disconnected are not replayed. No resend or sequence number is
  provided.

`event.id` is a ULID, so it embeds the millisecond at which the server
assigned it. It identifies an event; it MUST NOT be relied on for
cross-publisher ordering.

## 11. Backpressure policies

When a subscriber's per-conn send buffer (capacity 64) is full, a
`Policy` decides what happens to the event (`internal/conn/policy.go`):

| Policy             | Behaviour when buffer is full                                    |
| ------------------ | ---------------------------------------------------------------- |
| `PolicyDisconnect` | Returns `ErrSlowConsumer`: the event is not queued, and the conn is closed with **1008** (PolicyViolation), reason `slow consumer`. **The only policy the server uses.** |
| `PolicyDropOldest` | Evicts the head message and enqueues the new one. If the buffer is still full after the eviction (an ack or error frame took the freed slot), the new message is dropped. Never blocks, never errors. Not selectable. |
| `PolicyDropNewest` | Drops the new message. Never errors. Not selectable.             |

The policy is hardcoded to `PolicyDisconnect` in `cmd/wirefan/main.go`.
No flag, environment variable or per-channel setting selects another
one, so the two drop policies exist in code but a shipped binary never
uses them. Only the disconnect path counts drops: each event refused
there increments `wirefan_messages_dropped_total{reason="slow_consumer"}`,
while the drop policies count nothing.

No `error` frame is sent on the disconnect path: the client sees a
1008 close.

Acks (`subscribed`, `unsubscribed`) and `error` frames share the same
64-message buffer. If it is full when one of them is queued, that frame
is dropped silently; this does not disconnect the client.

## 12. Heartbeat

* The server sends a WebSocket `Ping` every **30 s** (`pingInterval`
  in `conn.go`).
* It waits up to **10 s** (`pongWait`) for the `Pong`. If the `Pong`
  does not arrive in time, the server drops the connection without a
  close frame, so the client sees **1006**. A dead or silent peer is
  therefore dropped within about 40 s.
* There is no read deadline. A client that answers pings stays
  connected indefinitely, even if it never sends a frame; listen-only
  clients need no application-level heartbeat.
* `Pong` handling is internal to `coder/websocket`; clients do not
  send or interpret heartbeat frames as JSON. Browsers running the
  W3C WebSocket API reply to pings automatically.
* Write timeout is **10 s** (`writeDeadline`); a write that does not
  finish in time ends the connection without a close frame (1006).

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
| POST   | `/v1/auth/sign`       | `Bearer <id>:<secret>` (prefix optional) | Sign a `private-` or `presence-` channel token. Body: `{socket_id, channel}`. Returns `{token}`. Status codes in §4. |
| GET    | `/v1/health`          | none                          | `200 ok` while serving; `503 draining` during shutdown. |
| GET    | `/`                   | none                          | Embedded demo client (web/).           |

Admin listener:

| Method | Path                  | Auth                          | Purpose                                |
| ------ | --------------------- | ----------------------------- | -------------------------------------- |
| POST   | `/v1/keys`            | `Bearer <admin_token>`        | Create API key. Body: `{"name": "..."}` (non-empty; unknown fields are refused with 400). Returns 201 `{id, name, secret}` (secret shown once). |
| GET    | `/v1/keys`            | `Bearer <admin_token>`        | List keys: `id`, `name`, `created_at`, and `revoked_at` once revoked. No secrets. |
| DELETE | `/v1/keys/{id}`       | `Bearer <admin_token>`        | Revoke a key: 204, or 404 for an unknown id. See below. |
| GET    | `/metrics`            | none (loopback-bound)         | Prometheus exposition.                 |
| ANY    | `/debug/pprof/*`      | none (loopback-bound)         | Standard `net/http/pprof` endpoints.   |

The `/v1/keys` routes require `Authorization: Bearer <admin_token>`,
with the exact `Bearer ` prefix, and answer 401 `unauthorized`
otherwise. `/metrics` and `/debug/pprof/*` check nothing; they rely
entirely on the admin listener being bound to loopback or an internal
network.

Revoking a key refuses it from then on: `/v1/connect` answers 401 and
`/v1/auth/sign` answers 401 for it. Every WebSocket still open with that
key is closed with **1008**, reason `key revoked`. Those closes run
asynchronously, so the 204 can arrive before they finish.

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

Compatibility rules for v1, as promised in
[`COMPATIBILITY.md`](COMPATIBILITY.md):

* Within 1.x, nothing listed there is removed or renamed, and nothing
  changes meaning. Minor releases may add optional fields to frames,
  new error codes and new frame types.
* Servers accept and ignore unknown JSON fields on inbound frames.
* Clients MUST ignore JSON fields they do not recognize, and MUST treat
  an unknown error `code` as a failure of the operation it answers.
* The subscribe token is an opaque string whose layout (§5.10) may
  change in any release.

A breaking protocol change would ship as **`v2`** on a new path
(`/v2/connect`) in a 2.0 release, with a published schedule for
retiring `v1`.
