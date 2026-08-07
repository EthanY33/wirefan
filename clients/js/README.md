# @wirefan/client

JavaScript/TypeScript client for the [wirefan](https://github.com/EthanY33/wirefan) WebSocket fan-out server. Zero runtime dependencies. Works in browsers and Node 20+ out of the box; any platform works if you inject a WebSocket implementation.

Not yet published to npm. Install from the repo:

```sh
cd clients/js && npm install && npm run build
# then depend on it locally, e.g.
npm install /path/to/wirefan/clients/js
```

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

- On any unexpected disconnect the client retries with exponential backoff plus jitter: delays start at 300 ms, double each attempt, cap at 15 s, and each is scaled by a random factor in [0.75, 1.25]. All parameters are settable via the `reconnect` option; `reconnect: false` disables retries entirely.
- After a reconnect the client automatically resubscribes every channel you were on and then emits `resubscribed`. Your handlers stay attached; you do nothing.
- The server issues a fresh `socket_id` per connection and subscribe tokens are single-use and socket-bound, so the client re-invokes your `authorize` callback for every `private-`/`presence-` resubscribe. Never cache tokens.
- An explicit `close()` never reconnects and is terminal: construct a new client to connect again.
- If `reconnect.maxAttempts` consecutive attempts fail, the client emits `closed` with reason `"exhausted"` and stops.
- The server pings every 30 s and drops peers that stay silent past its 60 s read deadline. Browser and Node WebSockets answer pings automatically, so an idle but healthy connection stays up with no work on your part; if the connection does die (proxy timeout, network change), the close event triggers the reconnect path above.
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
client.on("connected",    ({ socketId, reconnected }) => {});
client.on("disconnected", ({ code, reason, willReconnect }) => {});
client.on("reconnecting", ({ attempt, delayMs }) => {});
client.on("resubscribed", ({ channels }) => {});
client.on("state",        ({ state, previous }) => {}); // idle|connecting|connected|reconnecting|closed
client.on("error",        (err) => {});  // server errors not tied to an in-flight call
client.on("closed",       ({ reason }) => {}); // "explicit" | "exhausted"
```

Every `on()` returns an unsubscribe function.

## Errors

Failures are typed: `WirefanError` (a server `error` frame; `.code` carries the server's code, e.g. `AUTH_FAILED`, `RATE_LIMITED`, `RESERVED_CHANNEL`), `ConnectionClosedError`, `AckTimeoutError`, `ConfigurationError`. `subscribe()` rejects with the matching `WirefanError` when the server refuses; publish rejections (publish has no ack in the protocol) surface on the `error` event.

One caveat inherited from the wire protocol: server `error` frames carry no channel field, so the client attributes subscribe-class error codes to the oldest in-flight subscribe. With many concurrent subscribes racing publishes, attribution is heuristic; see the comment on `SUBSCRIBE_ERROR_CODES` in `src/index.ts`.

## Node / injection

Node 20+ has a global `WebSocket`; nothing to configure. Elsewhere (older Node, custom transports, tests), inject one:

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
