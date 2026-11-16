# 05. Client SDKs

Epic label: `epic:client-sdks`. Tracking issue: [#99](https://github.com/EthanY33/wirefan/issues/99). Milestones: v1.2, v1.3. Estimate: 57 h.

## Goal

Make `@wirefan/client` behave correctly at the edges (terminal server
answers, network changes, publishing during a reconnect), make it installable
from npm with typed payloads and a small bundle, cover the server half of
private-channel auth, and add first-party React and Go clients that follow
the same documented semantics.

## Current state

* **Package.** `clients/js/package.json`: version 0.1.0, `exports` has only
  `"."` with `import`, `files` is `["dist", "README.md"]`, no
  `publishConfig`, test script is `vitest run` with no type check. There is
  no `clients/js/LICENSE`. `clients/js/tsconfig.json` includes only `src`.
  README.md (around lines 191 and 320) says the client is not on npm and
  lists npm publishing as out of scope for 1.x.
* **Reconnect.** `DEFAULT_RECONNECT` in `clients/js/src/index.ts` (around
  line 358): 300 ms doubling to 15 s, jitter 0.25, `maxAttempts` infinite.
  `#onDrop` (lines 797-845) schedules the next attempt with `setTimeout`
  and ignores the close code and reason. The `closed` event reason is
  `"explicit" | "exhausted"` (line 258). No `online`, `offline`,
  `visibilitychange` or `pageshow` listener exists in `src`.
* **Terminal answers.** `docs/PROTOCOL.md` 8 documents close 1008 `key
  revoked`; 3.2 documents 401 at upgrade for a revoked or unknown key. The
  client retries both forever. The demo works around this with its own
  probe, `diagnoseFirstFailure` in `web/client.js` (lines 531-551).
* **Publish while reconnecting.** `publish()` (lines 1060-1067) throws
  `ConnectionClosedError` unless connected. The demo hand-rolls publish
  gating (`web/client.js` lines 37-38 and 176-186).
* **Types.** `EventFrame.data` and `ChannelEvent.data` are `unknown`
  (lines 34-40, 226-231); `publish(channel, data: unknown)`.
* **Bundle.** `dist/index.js` is 46,200 bytes unminified (about 9.7 KB
  gzip); `web/wirefan-client.js` is the vendored copy, checked for drift in
  `ci.yml` (lines 72-75).
* **Server half of auth.** The README private-channel section shows only the
  browser `authorize` callback with a placeholder backend route.
  `docs/PROTOCOL.md` 4 documents the `/v1/auth/sign` status codes every app
  reimplements.
* **React.** None. `close()` is terminal (lines 538-564), which breaks the
  naive `useEffect` create-then-close pattern under React StrictMode's
  double mount.
* **Go.** `clients/` contains only `js/`. The only Go client code is the
  raw dial and hand-built frames in `cmd/loadtest/main.go` (line 304 and
  around 326), with no reconnect, auth callback or typed errors.

## Proposed design

### Terminal answers (v1.2)

* In `#onDrop`, close 1008 with reason `key revoked` (and `key expired`
  once `03-key-management.md` ships it) is terminal: emit `disconnected`
  with `willReconnect: false`, then `closed` with reason `"key-revoked"`.
* Before-open failures: after two consecutive failures before `open`, the
  client (browser default on, off when `webSocket` is injected) sends one
  `fetch` to the HTTP form of the connect URL. A 401 closes with reason
  `"auth-failed"`. A 429 or 503 keeps reconnecting and sets
  `reconnecting.hint` to `"ip-cap"` or `"draining"`.
* `closed.reason` widens to
  `"explicit" | "exhausted" | "key-revoked" | "auth-failed"`. The client is
  0.x, so this is an allowed minor change; the README documents it.
* `web/client.js` drops `diagnoseFirstFailure` and uses the library's
  reasons and hints, keeping the bad-key and IP-cap overlays.

### Network-aware reconnect (v1.3)

`reconnect.onNetworkEvents` (default true when a global event target exists).
While `reconnecting`, an `online` event, a `pageshow` with `persisted`, or
`visibilitychange` to visible cancels the pending timer and dials at once,
with the delay reset to `initialDelayMs`, jitter kept, and a 1 s floor
since the last dial so a flapping network cannot hot-loop. Listeners are
registered on connect and removed by `close()` and on `exhausted`. An
injectable `eventTarget` option keeps tests deterministic. No client-side
ping is added.

### Publish outbox (v1.3)

```ts
new WirefanClient({ url, key, outbox: { maxMessages: 100, maxAgeMs: 10_000 } })
```

Off by default (`maxMessages: 0`). When on and the client is connecting or
reconnecting, `publish()` queues instead of throwing. After `#resubscribeAll`
re-confirms the channel, the queue flushes FIFO at 40 messages per second,
below the server's per-connection 50/s publish budget (`docs/PROTOCOL.md`
9). Entries older than `maxAgeMs` and entries over `maxMessages` are dropped
and reported on a new `dropped` event with a count. `close()` clears the
queue. The README states that this narrows, but does not remove, at-most-once
loss.

### Typed payloads and type-checked tests (v1.2)

```ts
type Channels = { "room-1": { msg: string; from: string } };
const c = new WirefanClient<Channels>({ url, key });
c.subscribe("room-1", (ev) => ev.data.msg);   // string
c.publish("room-1", { msg: 1 });              // compile error
```

`WirefanClient<Channels extends Record<string, unknown> = Record<string, unknown>>`;
the default keeps today's untyped behavior. An optional `parse(channel, data)`
hook lets users plug in a validator without a dependency. `tsconfig.test.json`
includes `test/`, the npm `test` script runs `tsc --noEmit -p
tsconfig.test.json` before vitest, and `expectTypeOf` tests pin the exported
surface.

### Bundle (v1.2)

esbuild (dev dependency) adds `dist/wirefan.min.js` (ESM, minified, source
map) and `dist/wirefan.global.js` (IIFE exposing `window.Wirefan`), exposed
as `./min` and `./global` in `exports` plus an `unpkg` field.
`scripts/vendor-web.mjs` vendors the minified build. A CI step fails if the
gzip size of `wirefan.min.js` exceeds the measured baseline plus 10 percent
and writes the number to the job summary.

### npm publish (v1.2)

* Copy `LICENSE` into `clients/js`, add it to `files`, add
  `publishConfig: { access: "public", provenance: true }` and a `prepack`
  build; a CI step runs `npm pack --dry-run` and asserts the tarball holds
  only `dist/`, `README.md`, `LICENSE` and `package.json`.
* `.github/workflows/release-client-js.yml` runs on `client-js-v*` tags,
  reuses the client CI job, then `npm publish --provenance` through npm
  trusted publishing (OIDC, no stored token). The server workflow's `v*`
  filter is narrowed so client tags do not trigger it.
* `clients/js/CHANGELOG.md` and a versioning rule (client 0.x, independent
  of server tags).
* README, `clients/js/README.md`, `CONTRIBUTING.md`, `CLAUDE.md` and
  `docs/DESIGN.md` (line 30) switch to `npm install @wirefan/client`.
* First publish is a prerelease (`0.2.0-rc.0`) under the `next` dist-tag.

Owner prerequisite: create the `@wirefan` npm scope and configure trusted
publishing for the repository.

### Server-side helper (v1.3)

Subpath export `@wirefan/client/server`, using global `fetch` only, safe
for Node, Bun, Deno and edge runtimes:

```ts
import { createSigner, authRoute } from "@wirefan/client/server";

const signer = createSigner({ url: "https://wirefan.example.com", keyId, keySecret });
export const POST = authRoute(signer, {
  authorize: async (req, { socketId, channel }) => userCanJoin(req, channel),
});
```

`authRoute` returns a Fetch-API `(Request) => Promise<Response>` handler.
The `authorize` callback is required (no default allow); a denied check
returns 403 without calling `/v1/auth/sign`; sign failures surface as a
typed `SignError` with the HTTP status. Importing the module where `window`
is defined throws a `ConfigurationError`, so the secret cannot be bundled
into a browser build by accident.

### React hooks (v1.3)

New package `clients/react` (`@wirefan/react`), peer dependencies `react
>=18` and `@wirefan/client`:

* `<WirefanProvider client={...}>` or `options={...}` owns the client. On
  unmount it defers `close()` to a cancellable timer so StrictMode's
  unmount-remount reuses the same client.
* `useWirefanState()` via `useSyncExternalStore` over the `state` event.
* `useChannel<T>(channel, handler?, { enabled })` returns `{ lastEvent }`
  and shares one subscription across components through the client's
  existing multi-handler channel records.
* `usePublish()`.

Released by the same tag workflow as the JS client, with its own tag prefix.

### Go client (v1.3)

Separate module `clients/go` (`github.com/EthanY33/wirefan/clients/go`,
its own `go.mod`), so it is versioned independently and does not widen the
server's 1.x promise. Built on `github.com/coder/websocket`.

```go
c, err := wirefan.Dial(ctx, "wss://host/v1/connect", keyID,
    wirefan.WithAuthorize(func(ctx context.Context, socketID, channel string) (string, error) { ... }),
    wirefan.WithReconnect(wirefan.DefaultReconnect))
sub, err := c.Subscribe(ctx, "room-1", func(ev wirefan.Event) { ... })
err = c.Publish(ctx, "room-1", map[string]any{"msg": "hi"})
```

Reconnect defaults mirror the JS client (300 ms doubling to 15 s, jitter
0.25), resubscribe uses fresh tokens, errors are `*wirefan.Error{Code, Op,
Channel}`, terminal answers match the JS behavior above, and `Close`
honors context. Tests run against an in-process server through a `replace`
directive in the test module; a new CI job runs `go test -race` in
`clients/go`. `cmd/loadtest` moves to it only if the benchmark numbers do
not change.

## Protocol and API changes

None on the server. All changes are in client packages:

| Package | Change |
| ------- | ------ |
| `@wirefan/client` | New `closed` reasons, `reconnecting.hint`, `reconnect.onNetworkEvents`, `eventTarget`, `outbox`, `dropped` event, generic `Channels` parameter, `parse` hook, `./min`, `./global` and `./server` exports, `publishWithAck` (from `02-protocol-api.md`) |
| `@wirefan/react` | New package |
| `github.com/EthanY33/wirefan/clients/go` | New module |

## Compatibility impact

`docs/COMPATIBILITY.md` excludes the JS client API from the server promise;
it is 0.x and may change in a minor. Even so, every change here is additive
or opt-in except the widened `closed.reason` union, which a TypeScript
`switch` with an exhaustive check would notice; the CHANGELOG calls it out.
All clients speak wire protocol v1 and work with every 1.x server.

## Task breakdown

| Order | Issue | Milestone | Est. |
| ----- | ----- | --------- | ---- |
| 1 | [#71](https://github.com/EthanY33/wirefan/issues/71) | v1.2 | 6 h |
| 2 | [#72](https://github.com/EthanY33/wirefan/issues/72) | v1.2 | 5 h |
| 3 | [#73](https://github.com/EthanY33/wirefan/issues/73) | v1.2 | 4 h |
| 4 | [#74](https://github.com/EthanY33/wirefan/issues/74) | v1.2 | 6 h |
| 5 | [#81](https://github.com/EthanY33/wirefan/issues/81) | v1.3 | 4 h |
| 6 | [#82](https://github.com/EthanY33/wirefan/issues/82) | v1.3 | 5 h |
| 7 | [#83](https://github.com/EthanY33/wirefan/issues/83) | v1.3 | 5 h |
| 8 | [#84](https://github.com/EthanY33/wirefan/issues/84) | v1.3 | 8 h |
| 9 | [#85](https://github.com/EthanY33/wirefan/issues/85) | v1.3 | 14 h |

Issues 2 and 3 land before 4 so the first published package already has
its final exports. Issue 1 lands after the demo browser test in
`08-testing.md`, which guards the overlays it touches.

## Test plan

* Terminal answers: integration test revokes a key through the admin API;
  the connected client emits `closed { reason: "key-revoked" }` and makes no
  further dial; a client built with a revoked key closes with `auth-failed`.
* Network events: with a fake event target, `online` while reconnecting
  dials within 1 s; repeated events inside the floor produce one dial;
  `close()` removes every listener.
* Outbox: three publishes during a reconnect arrive in order after
  resubscribe (real server); overflow and stale entries are dropped and
  counted; a 200-message flush never triggers `RATE_LIMITED_CONN`.
* Types: `expectTypeOf` tests; `npm test` fails on a type error in a test
  file; `dist/index.d.ts` keeps every existing export.
* Bundle: the IIFE build works from a plain script tag in the demo smoke
  page; the size gate runs in CI.
* npm: `npm pack --dry-run` content check; a server `v*` tag does not
  trigger the client workflow and the reverse.
* Server helper: a token minted through `authRoute` subscribes a browser
  client to a `private-` channel on the real server; denied checks never
  reach `/v1/auth/sign`.
* React: under StrictMode exactly one WebSocket and one subscribe frame;
  two components on one channel share one subscription; last unmount
  unsubscribes; provider unmount closes once.
* Go: package example runs; killing and restarting the server reconnects and
  resubscribes; private channel works through `WithAuthorize` across a
  reconnect; `go test -race` passes in CI.

## Open questions

* Should the outbox drop messages queued for a channel whose resubscribe
  fails definitively, and report them on `dropped`? Proposed: yes.
* Go client: expose a channel-of-events API in addition to callbacks?
  Proposed: callbacks only at first, matching the JS client.
* Should `@wirefan/react` wait for the JS client's own 1.0 before its first
  publish? Proposed: publish as 0.x alongside the client.
