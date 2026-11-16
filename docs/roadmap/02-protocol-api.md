# 02. Protocol and API

Epic label: `epic:protocol-api`. Tracking issue: [#96](https://github.com/EthanY33/wirefan/issues/96). Milestones: v1.1 (text-only fix), v1.2
(the rest). Estimate: 21 h.

## Goal

1. Let an app server publish to a channel with one HTTP request instead of
   holding a subscribed WebSocket.
2. Let a WebSocket publisher learn the id the server assigned to its event
   and match failures to the publish that caused them.
3. Enforce the text-only rule that `docs/PROTOCOL.md` section 2 already
   states.
4. Describe the REST API in a machine-readable spec that cannot drift from
   the router.

## Current state

* **Publish needs a socket.** `internal/conn/handler.go: handlePublish`
  (around lines 212-225) refuses with `NOT_SUBSCRIBED` unless the
  connection holds the channel. `docs/PROTOCOL.md` 5.7: the publisher
  receives its own event, with no echo suppression. The public REST routes
  are only `POST /v1/auth/sign` (`internal/server/rest.go: RegisterPublic`,
  around line 66); admin routes are `/v1/keys` (`RegisterAdmin`, lines
  73-77). Route wiring is in `internal/server/server.go` (lines 89-116).
* **Publish is unacknowledged.** `handlePublish` mints the event id with
  `ulid.Make().String()` (around line 234) and builds the frame with
  `marshalFrame` (around line 288). `docs/PROTOCOL.md` 5.7: "A successful
  publish is not acknowledged."
* **Binary frames are processed.** `internal/conn/pumps.go: readPump`
  (line 52) does `_, raw, err := c.ws.Read(ctx)` and discards the message
  type, so a binary frame containing JSON is handled like a text frame.
  `docs/PROTOCOL.md` 2 says the transport is text only and clients MUST send
  text. The JS client drops non-string frames on receipt
  (`clients/js/src/index.ts`, `#onMessage`).
* **No machine-readable spec.** No `openapi.yaml` exists. The REST API is
  documented as prose tables in `docs/PROTOCOL.md` sections 4 and 13.

## Proposed design

### `POST /v1/publish`

Mounted in `RestHandler.RegisterPublic` next to `/v1/auth/sign`.

```
POST /v1/publish
Authorization: Bearer <key_id>:<key_secret>
Content-Type: application/json

{
  "channel": "private-room42",
  "data": { "msg": "deploy finished" },
  "exclude_socket_id": "01HKQ8M9F7XPQVJZ4YCZ7G3W2A"
}
```

```
HTTP/1.1 202 Accepted
Content-Type: application/json

{ "id": "01HKQ8N2C4T9V6M1R8E5JX0B7D" }
```

| Status | Body | Cause |
| ------ | ---- | ----- |
| 202 | `{id}` | Accepted and handed to the fanout, including when the channel has no subscribers. |
| 400 | plain text | Body not JSON, unknown field, invalid channel name (same rules as `conn.ValidateChannelName`), `_`-prefixed channel, or a malformed `exclude_socket_id`. |
| 401 | `bad credentials` | Same check as `/v1/auth/sign`. |
| 413 | plain text | Body over 64 KiB, the same cap as an inbound WebSocket message. |
| 429 | plain text, `Retry-After` | The key's shared bucket is empty. |

Implementation:

* Factor the credential check out of `sign` into `authKey(r) (store.Key, bool)`.
* Extract `conn.BuildEvent(channel string, data json.RawMessage) (id string, frame []byte)`
  from `handlePublish`, so REST and WebSocket publishes produce
  byte-identical frames.
* `NewRestHandler` gains the `Registry`, `Fanout` and `RateLimit`
  dependencies. The handler calls `registry.GetOrCreate` (or a lookup that
  does not create when there are no subscribers), then `fanout.Broadcast`,
  and records `metrics.Published` and `metrics.Latency` as the socket path
  does.
* `exclude_socket_id` skips one subscriber during the hub-level send loop.
  This is the common pattern of "an app server relays a browser's action
  to everyone except that browser".
* Key scopes from `03-key-management.md` apply here once they land: a
  subscribe-only key is refused.

### Publish ack

Opt-in through a new optional `ref` field on `publish`. Without `ref`,
nothing changes and no new frame is sent.

```json
{ "type": "publish", "channel": "chat", "data": { "msg": "hi" }, "ref": "p-17" }
```

```json
{ "type": "published", "channel": "chat", "ref": "p-17", "id": "01HKQ8N2C4T9V6M1R8E5JX0B7D" }
```

The `published` frame is queued after `fanout.Broadcast` returns. It is an
ack frame and, like other acks (`docs/PROTOCOL.md` 11), can be dropped when
the send buffer is full. An `error` frame answering a publish that carried
`ref` echoes it:

```json
{ "type": "error", "code": "RATE_LIMITED_CONN", "message": "too many publishes on this connection", "op": "publish", "channel": "chat", "ref": "p-17" }
```

`ref` is a string of at most 64 bytes. A longer `ref` is treated as absent
(the publish proceeds, no ack is sent and errors do not echo it), so a
client bug cannot make the server copy large strings into ack frames. In
`@wirefan/client`,
`publish()` keeps its fire-and-forget signature, and a new
`publishWithAck(channel, data): Promise<string>` sends a generated `ref`
and resolves with the id, reusing the `#pending` / `#settleOp` machinery in
`clients/js/src/index.ts`.

### Text-only inbound frames

`readPump` captures the message type. A `websocket.MessageBinary` frame is
answered with a new error code and not processed:

```json
{ "type": "error", "code": "BAD_FRAME", "message": "binary frames are not supported" }
```

The connection stays open, matching the rule in `docs/PROTOCOL.md` 7 that
error frames never close it. `BAD_FRAME` carries no `op` or `channel`, like
`BAD_JSON`.

### OpenAPI spec

`docs/openapi.yaml` (OpenAPI 3.1) with two `servers` entries (public and
admin listener), the two bearer schemes, every status code and plain-text
body from `docs/PROTOCOL.md` 4 and 13, and the 101/401/403/429/503 answers
of `/v1/connect`. A Go test in `internal/server` parses the spec and asserts
that its method and path pairs equal the routes registered by
`RegisterPublic`, `RegisterAdmin` and `server.go`, so a route added without
a spec entry fails `go test`. A pinned linter (Redocly CLI or Spectral)
runs in `ci.yml`. The spec lands before `POST /v1/publish`, so the new route
is written into it as part of that issue.

## Protocol and API changes

| Change | Kind under COMPATIBILITY.md |
| ------ | --------------------------- |
| `POST /v1/publish` on the public listener | New path |
| Optional `ref` on `publish` | New optional field |
| New frame type `published` (Server to Client) | New frame type |
| Optional `ref` on `error` | New optional field |
| New error code `BAD_FRAME` | New error code |
| `docs/openapi.yaml` | Documentation |

`docs/PROTOCOL.md` changes: the direction table in 5.1 gains `published`;
5.7 describes `ref`; 7 gains `BAD_FRAME`; 2 states that binary frames are
answered with `BAD_FRAME`; 13 gains `POST /v1/publish`.

## Compatibility impact

All four changes are additions allowed in a 1.x minor. A 1.0 client never
sends `ref`, so it never receives `published`. Clients already must treat an
unknown error code as a failure (`docs/PROTOCOL.md` 7, 14). A client that
sent binary frames was already violating a MUST in `docs/PROTOCOL.md` 2;
the CHANGELOG lists the change as a conformance fix.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#48](https://github.com/EthanY33/wirefan/issues/48) | v1.1 | 2 h |
| 2 | [#65](https://github.com/EthanY33/wirefan/issues/65) | v1.2 | 5 h |
| 3 | [#66](https://github.com/EthanY33/wirefan/issues/66) | v1.2 | 8 h |
| 4 | [#67](https://github.com/EthanY33/wirefan/issues/67) | v1.2 | 6 h |

## Test plan

* `handler_test.go`: a binary frame containing a valid subscribe produces
  `BAD_FRAME` and no subscription; text frames are unaffected.
* REST publish: a subscriber on a WebSocket receives an `event` byte-identical
  to one produced by a WebSocket publish with the same data (golden
  comparison of the frame minus `id`); 401, 400, 413 and 429 cases;
  publishing to an empty channel returns 202; `exclude_socket_id` suppresses
  exactly one delivery; a race test runs REST publishes concurrently with
  WebSocket subscribe and unsubscribe under `-race`.
* Publish ack: no `published` frame without `ref`; exactly one with `ref`,
  whose `id` equals the `event.id` subscribers saw; error frames echo `ref`;
  JS unit tests on the fake WebSocket and one integration test against the
  real server.
* OpenAPI: the route-parity test fails when a route is added without a spec
  entry (checked by temporarily adding one); the linter passes in CI.

## Open questions

* Should REST publish create a channel entry in the registry when it has
  no subscribers, or skip the fanout entirely? Skipping avoids churn for
  the sweeper; creating keeps one code path.
* Should `exclude_socket_id` require that the socket was opened with the
  calling key, for the same reason as the sign binding in
  `01-abuse-resistance.md`? Excluding a foreign socket only hides one
  delivery from it, so the risk is low.
* Should `published` be sent before or after the broadcast loop finishes?
  After is simpler to reason about; before reduces ack latency on large
  channels under `--fanout=per-conn`.
