# @wirefan/client

JavaScript/TypeScript client for the [wirefan](https://github.com/EthanY33/wirefan) WebSocket fan-out server. Zero runtime dependencies. Runs in browsers and on Node 22 or later out of the box. Node 22+ is required (`engines` is `>=22`; 22 is the first line with a global `WebSocket` enabled by default). A custom WebSocket implementation can be injected for tests or other runtimes (see [Node / injection](#node--injection)).

Not yet published to npm. Install from the repo:

```sh
cd clients/js && npm install && npm run build
# then depend on it locally, e.g.
npm install /path/to/wirefan/clients/js
```

## Compatibility

- Speaks wire protocol `v1` ([docs/PROTOCOL.md](../../docs/PROTOCOL.md)) and works with wirefan server 1.x.
- The client API itself is 0.x (currently 0.1.0): it may change in minor versions until it reaches 1.0.
- Requires Node 22+ outside the browser; see [Node / injection](#node--injection).

## Quickstart

```js
import { WirefanClient } from "@wirefan/client";

const client = new WirefanClient({
  url: "wss://relay.example.com",   // path defaults to /v1/connect
  key: "01K...",                    // API key id from POST /v1/keys
});

await client.connect();
const sub = await client.subscribe("demo", (ev) => {
  console.log(ev.channel, ev.data, ev.id);
});
client.publish("demo", { hello: "world" });
// later:
await sub.unsubscribe();
client.close();
```

## Reconnect semantics (read this)

- On any unexpected disconnect the client retries with exponential backoff plus jitter: delays start at 300 ms, double each attempt, cap at 15 s, and each is scaled by a random factor in [0.75, 1.25]. All parameters are settable via the `reconnect` option.
- `reconnect: false` makes the client single-use: the first unexpected disconnect emits `closed` with reason `"exhausted"` (no attempts are made) and permanently closes the client; any later `connect()` rejects with `client is closed; create a new WirefanClient`.
- Each dial is bounded by `handshakeTimeoutMs` (default 10 s), covering the upgrade plus the wait for the server's `connected` frame. A connection that upgrades but never becomes ready is torn down and fed to the same backoff path.
- After a reconnect the client automatically resubscribes every channel you were on and then emits `resubscribed`. Your handlers stay attached; you do nothing.
- A resubscribe that fails does not silently cost you the channel. What happens depends on the failure:
  - The connection drops again mid-restore: the channel is kept and the next connection restores it (no `error` event; `disconnected` already told you).
  - A transient failure (the ack times out, the server answers `RATE_LIMITED` or `RATE_LIMITED_CONN` because a reconnect herd hit its rate limit, your `authorize()` throws, or a code this client does not know): the client emits `error`, keeps the channel, and retries on the same connection with the reconnect backoff curve. Once a retry lands, the channel gets its own `resubscribed` event. Retries stop when the connection drops (the next connection starts over), when you unsubscribe, or on `close()`.
  - A definitive refusal (`AUTH_FAILED`, `AUTH_REPLAYED`, `RESERVED_CHANNEL`, `BAD_CHANNEL`, any `LIMIT_*` code, `SUBSCRIBE_FAILED`): the client emits `error` and drops the channel; its handlers stop and its `Subscription.active` turns false for good (subscribing again returns a new handle).
- The server issues a fresh `socket_id` per connection and subscribe tokens are single-use and socket-bound, so the client re-invokes your `authorize` callback for every `private-`/`presence-` resubscribe. Never cache tokens. This holds even when the drop lands while an `authorize()` call is still in flight: the stale attempt is abandoned (a connection-epoch check stops its token from ever reaching the new socket) and the resubscribe fetches a fresh token bound to the new `socket_id`.
- If that interrupted attempt was your own `subscribe()` call and the client has reconnected by the time it fails, the call adopts the resubscribe instead of rejecting: it resolves with your handler attached once the channel is confirmed on the new connection, or rejects with the refusal if the server turns it down (the same error the `error` event reports, even when the refusal lands before the interrupted attempt fails). It rejects with `ConnectionClosedError` only when the client is not connected again yet, and then your handler is removed.
- An explicit `close()` never reconnects and is terminal: construct a new client to connect again.
- If `reconnect.maxAttempts` consecutive attempts fail, the client emits `closed` with reason `"exhausted"` and stops. The client is then permanently closed, exactly as with `reconnect: false` above.
- Keepalive: the server pings every 30 s and disconnects a peer that does not answer a ping within 10 s. Browsers and Node answer pings automatically (below the JavaScript API), so a healthy idle connection stays up with no work on your part and no application-level heartbeat. If the connection does die (proxy timeout, network change, a peer that stopped answering), the close event triggers the reconnect path above.
- Delivery is at-most-once and only per-subscriber FIFO is guaranteed. A reconnect window loses whatever was published while you were away; there is no replay.

## Private and presence channels

Subscribing to `private-*` or `presence-*` requires a token your app server obtains from wirefan's `POST /v1/auth/sign` (the API key secret must never reach the browser). Provide a callback:

```js
const client = new WirefanClient({
  url: "wss://relay.example.com",
  key: "01K...",
  authorize: async ({ socketId, channel }) => {
    const res = await fetch("/my-backend/wirefan-auth", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ socket_id: socketId, channel }),
    });
    const { token } = await res.json();
    return token;
  },
});
```

The callback runs once per subscribe attempt, including automatic resubscribes.

## Events

```js
client.on("connected",    ({ socketId, reconnected }) => {}); // reconnected is false for the first successful connection
client.on("disconnected", ({ code, reason, willReconnect }) => {});
client.on("reconnecting", ({ attempt, delayMs }) => {});
client.on("resubscribed", ({ channels }) => {}); // after the restore pass, then once per channel a retry restored
client.on("state",        ({ state, previous }) => {}); // idle|connecting|connected|reconnecting|closed
client.on("error",        (err) => {});  // server errors not tied to an in-flight call, and resubscribe failures
client.on("closed",       ({ reason }) => {}); // "explicit" | "exhausted"
```

Every `on()` returns an unsubscribe function. `reconnected` stays false on the first successful connection even when earlier dials failed; it is true only after a connection that had been up drops and comes back.

## Errors

Failures are typed: `WirefanError` (a server `error` frame; `.code` carries the server's code, e.g. `AUTH_FAILED`, `RATE_LIMITED`, `RESERVED_CHANNEL`), `ConnectionClosedError`, `AckTimeoutError`, `ConfigurationError`. `subscribe()` rejects with the matching `WirefanError` when the server refuses, and so does `Subscription.unsubscribe()`; publish rejections (publish has no ack in the protocol) surface on the `error` event.

Error routing depends on the server:

- Servers that name the frame an error answers (the optional `op` and `channel` fields on `error` frames) are routed exactly: the error settles the pending `subscribe` or `unsubscribe` for that channel, and anything else (every publish rejection, or an operation that is no longer pending) goes to the `error` event. `WirefanError.op` and `WirefanError.channel` carry those fields, so you can tell which publish was refused.
- Older servers send neither field. The client then falls back to attributing subscribe-class codes to the oldest in-flight subscribe; with many concurrent subscribes racing publishes, that attribution is heuristic (see `SUBSCRIBE_ERROR_CODES` in `src/index.ts`), and an unsubscribe the server refuses waits for its ack timeout.

Two consequences worth knowing:

- An unsubscribe the server refuses (for example `RATE_LIMITED`) rejects, but the client has already forgotten the channel, so events that still arrive for it are dropped until the connection ends.
- A subscribe that timed out (`AckTimeoutError`) is forgotten locally. If its ack arrives late, the client sends an unsubscribe so the server does not keep a subscription nobody holds.

## Node / injection

Node 22+ is required and enables the global `WebSocket` by default; nothing to configure there. For tests, a custom transport, or another runtime without a usable global `WebSocket`, inject an implementation:

```js
import WebSocket from "ws";
const client = new WirefanClient({ url, key, webSocket: WebSocket });
```

## Tests

```sh
npm test
```

Unit tests run against a scripted in-memory fake WebSocket (deterministic reconnect/backoff coverage). The integration suite boots the real Go server and is skipped, with a visible notice, unless a `wirefan` binary exists at the repo root (or `WIREFAN_BIN` points to one):

```sh
go build -o wirefan.exe ./cmd/wirefan   # repo root
cd clients/js && npm test
```
