/**
 * @wirefan/client: JavaScript client for the wirefan WebSocket fan-out server.
 *
 * Wire protocol: docs/PROTOCOL.md (protocol id "v1").
 *
 * Single-file on purpose: the compiled output is one dependency-free ESM
 * module, which lets the repo's demo page vendor it without a bundler.
 */

// ---------------------------------------------------------------------------
// Wire protocol types (docs/PROTOCOL.md §5)
// ---------------------------------------------------------------------------

/** Server -> client: first frame after the upgrade (§5.2). */
export interface ConnectedFrame {
  type: "connected";
  socket_id: string;
  version: string;
}

/** Server -> client: subscribe acknowledged (§5.4). */
export interface SubscribedFrame {
  type: "subscribed";
  channel: string;
}

/** Server -> client: unsubscribe acknowledged (§5.6). */
export interface UnsubscribedFrame {
  type: "unsubscribed";
  channel: string;
}

/** Server -> client: a published message fanned out to this subscriber (§5.8). */
export interface EventFrame {
  type: "event";
  channel: string;
  data: unknown;
  id: string;
}

/** Server -> client: a request was rejected; the connection stays open (§5.9). */
export interface ErrorFrame {
  type: "error";
  code: string;
  message: string;
}

export type ServerFrame =
  | ConnectedFrame
  | SubscribedFrame
  | UnsubscribedFrame
  | EventFrame
  | ErrorFrame;

/** Error codes the server emits (§7). Unknown codes must be tolerated. */
export type WirefanErrorCode =
  | "BAD_JSON"
  | "BAD_CHANNEL"
  | "BAD_TYPE"
  | "AUTH_FAILED"
  | "AUTH_REPLAYED"
  | "NOT_SUBSCRIBED"
  | "RATE_LIMITED"
  | "RATE_LIMITED_CONN"
  | "RESERVED_CHANNEL"
  | "LIMIT_CHANNELS"
  | "LIMIT_SUBSCRIBERS"
  | "SUBSCRIBE_FAILED"
  | (string & {});

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/** A server `error` frame surfaced as a typed exception. */
export class WirefanError extends Error {
  readonly code: WirefanErrorCode;
  constructor(code: WirefanErrorCode, message: string) {
    super(`${code}: ${message}`);
    this.name = "WirefanError";
    this.code = code;
  }
}

/** The operation could not complete because the connection dropped or closed. */
export class ConnectionClosedError extends Error {
  readonly closeCode: number | undefined;
  readonly reason: string | undefined;
  constructor(message: string, closeCode?: number, reason?: string) {
    super(message);
    this.name = "ConnectionClosedError";
    this.closeCode = closeCode;
    this.reason = reason;
  }
}

/** A subscribe/unsubscribe acknowledgement did not arrive in time. */
export class AckTimeoutError extends Error {
  constructor(op: string, channel: string, ms: number) {
    super(`${op} "${channel}" not acknowledged within ${ms}ms`);
    this.name = "AckTimeoutError";
  }
}

/** The client was used in a way its configuration cannot support. */
export class ConfigurationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigurationError";
  }
}

// ---------------------------------------------------------------------------
// WebSocket abstraction (injectable so Node, browsers, and tests all work)
// ---------------------------------------------------------------------------

export interface WebSocketLike {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev?: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code?: number; reason?: string }) => void) | null;
  onerror: ((ev?: unknown) => void) | null;
}

export type WebSocketConstructor = new (url: string) => WebSocketLike;

const WS_OPEN = 1;

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

export interface ReconnectOptions {
  /** Delay before the first retry. Default 300 ms. */
  initialDelayMs?: number;
  /** Upper bound on the delay between retries. Default 15 000 ms. */
  maxDelayMs?: number;
  /** Exponential growth factor. Default 2. */
  multiplier?: number;
  /**
   * Full-jitter fraction in [0, 1]; each delay is scaled by a random factor
   * in [1 - jitter, 1 + jitter]. Default 0.25.
   */
  jitter?: number;
  /** Give up (and close the client) after this many consecutive failed attempts. Default Infinity. */
  maxAttempts?: number;
}

export interface AuthorizeContext {
  socketId: string;
  channel: string;
}

/**
 * Fetches a subscribe token for a `private-` or `presence-` channel.
 * Called once per subscribe attempt, including automatic resubscribes after a
 * reconnect: tokens are single-use and bound to the current socket_id, so a
 * cached token is never valid twice.
 */
export type AuthorizeFn = (ctx: AuthorizeContext) => Promise<string> | string;

export interface WirefanClientOptions {
  /**
   * Server URL. Accepts ws://, wss://, http://, or https:// (http(s) is
   * rewritten to ws(s)). The path defaults to /v1/connect when the URL has
   * no path.
   */
  url: string;
  /** API key id (the `id` from POST /v1/keys). Sent as the `key` query param. */
  key: string;
  /** Token fetcher for private-/presence- channels. */
  authorize?: AuthorizeFn;
  /** WebSocket implementation. Defaults to globalThis.WebSocket. */
  webSocket?: WebSocketConstructor;
  /** Reconnect tuning, or `false` to disable automatic reconnection. */
  reconnect?: ReconnectOptions | false;
  /** How long to wait for a subscribe/unsubscribe ack. Default 10 000 ms. */
  ackTimeoutMs?: number;
  /** Random source for backoff jitter; injectable for deterministic tests. */
  random?: () => number;
}

// ---------------------------------------------------------------------------
// Public event model
// ---------------------------------------------------------------------------

export type WirefanState =
  | "idle"
  | "connecting"
  | "connected"
  | "reconnecting"
  | "closed";

/** A message delivered to a channel handler. */
export interface ChannelEvent {
  channel: string;
  data: unknown;
  /** Server-assigned ULID for this event. */
  id: string;
}

export interface ClientEvents {
  /** Any state transition. */
  state: { state: WirefanState; previous: WirefanState };
  /** Connection is ready (fired on first connect and on every reconnect). */
  connected: { socketId: string; reconnected: boolean };
  /** The transport dropped. `willReconnect` says whether a retry is scheduled. */
  disconnected: { code?: number; reason?: string; willReconnect: boolean };
  /** A reconnect attempt is scheduled. */
  reconnecting: { attempt: number; delayMs: number };
  /** After a reconnect, all previous channels were resubscribed. */
  resubscribed: { channels: string[] };
  /**
   * A server error frame that does not belong to an in-flight operation,
   * or a resubscribe failure after reconnect.
   */
  error: Error;
  /** The client is permanently closed (explicit close() or retries exhausted). */
  closed: { reason: "explicit" | "exhausted" };
}

type Handler<T> = (payload: T) => void;

/** A live channel subscription handle returned by `subscribe()`. */
export interface Subscription {
  readonly channel: string;
  /** True until `unsubscribe()` is called or the client closes. */
  readonly active: boolean;
  /**
   * Remove this handle's handler and, when it is the channel's last handle,
   * send an `unsubscribe` frame and await the ack.
   */
  unsubscribe(): Promise<void>;
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

interface PendingOp {
  kind: "subscribe" | "unsubscribe";
  channel: string;
  resolve: () => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

interface ChannelRecord {
  handlers: Set<Handler<ChannelEvent>>;
  /** Confirmed by a `subscribed` ack on the current connection. */
  confirmed: boolean;
  /** Shared by concurrent subscribe() calls so only one frame is sent. */
  inflight: Promise<void> | null;
}

/**
 * Error codes that answer a `subscribe` request. The server's error frames
 * carry no channel field (§5.9), so attribution is by code class + FIFO order:
 * the server processes one inbound frame at a time, so replies to control ops
 * arrive in the order the ops were sent.
 */
const SUBSCRIBE_ERROR_CODES = new Set<string>([
  "AUTH_FAILED",
  "AUTH_REPLAYED",
  "BAD_CHANNEL",
  "RESERVED_CHANNEL",
  "LIMIT_CHANNELS",
  "LIMIT_SUBSCRIBERS",
  "SUBSCRIBE_FAILED",
  "RATE_LIMITED",
]);

const DEFAULT_RECONNECT: Required<ReconnectOptions> = {
  initialDelayMs: 300,
  maxDelayMs: 15_000,
  multiplier: 2,
  jitter: 0.25,
  maxAttempts: Number.POSITIVE_INFINITY,
};

function needsToken(channel: string): boolean {
  return channel.startsWith("private-") || channel.startsWith("presence-");
}

function buildUrl(raw: string, key: string): string {
  let url = raw;
  if (url.startsWith("https://")) url = "wss://" + url.slice(8);
  else if (url.startsWith("http://")) url = "ws://" + url.slice(7);
  const u = new URL(url);
  if (u.pathname === "/" || u.pathname === "") u.pathname = "/v1/connect";
  u.searchParams.set("key", key);
  return u.toString();
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

export class WirefanClient {
  readonly #url: string;
  readonly #authorize: AuthorizeFn | undefined;
  readonly #WS: WebSocketConstructor;
  readonly #reconnect: Required<ReconnectOptions> | false;
  readonly #ackTimeoutMs: number;
  readonly #random: () => number;

  #ws: WebSocketLike | null = null;
  #state: WirefanState = "idle";
  #socketId: string | null = null;
  #closed = false;
  #attempt = 0;
  #reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  #channels = new Map<string, ChannelRecord>();
  #pending: PendingOp[] = [];
  #listeners = new Map<keyof ClientEvents, Set<Handler<never>>>();

  #connectWaiters: { resolve: () => void; reject: (e: Error) => void }[] = [];

  constructor(options: WirefanClientOptions) {
    if (!options.url) throw new ConfigurationError("url is required");
    if (!options.key) throw new ConfigurationError("key is required");
    this.#url = buildUrl(options.url, options.key);
    this.#authorize = options.authorize;
    const WS =
      options.webSocket ??
      (globalThis as { WebSocket?: WebSocketConstructor }).WebSocket;
    if (!WS) {
      throw new ConfigurationError(
        "no WebSocket implementation: pass options.webSocket (e.g. from the 'ws' package) on platforms without a global WebSocket",
      );
    }
    this.#WS = WS;
    this.#reconnect =
      options.reconnect === false
        ? false
        : { ...DEFAULT_RECONNECT, ...(options.reconnect ?? {}) };
    this.#ackTimeoutMs = options.ackTimeoutMs ?? 10_000;
    this.#random = options.random ?? Math.random;
  }

  /** Current lifecycle state. */
  get state(): WirefanState {
    return this.#state;
  }

  /** The server-issued socket id, or null while not connected. */
  get socketId(): string | null {
    return this.#socketId;
  }

  // ----- events ------------------------------------------------------------

  /** Register a listener. Returns a function that removes it. */
  on<K extends keyof ClientEvents>(
    event: K,
    handler: Handler<ClientEvents[K]>,
  ): () => void {
    let set = this.#listeners.get(event);
    if (!set) {
      set = new Set();
      this.#listeners.set(event, set);
    }
    set.add(handler as Handler<never>);
    return () => this.off(event, handler);
  }

  off<K extends keyof ClientEvents>(
    event: K,
    handler: Handler<ClientEvents[K]>,
  ): void {
    this.#listeners.get(event)?.delete(handler as Handler<never>);
  }

  #emit<K extends keyof ClientEvents>(event: K, payload: ClientEvents[K]): void {
    const set = this.#listeners.get(event);
    if (!set) return;
    for (const h of [...set]) {
      try {
        (h as Handler<ClientEvents[K]>)(payload);
      } catch {
        // A throwing listener must not break dispatch to the others.
      }
    }
  }

  #setState(next: WirefanState): void {
    if (next === this.#state) return;
    const previous = this.#state;
    this.#state = next;
    this.#emit("state", { state: next, previous });
  }

  // ----- lifecycle ---------------------------------------------------------

  /**
   * Open the connection. Resolves once the server's `connected` frame arrives.
   * With reconnect enabled the promise stays pending across failed attempts
   * and rejects only when retries are exhausted or the client is closed.
   */
  connect(): Promise<void> {
    if (this.#closed) {
      return Promise.reject(
        new ConnectionClosedError("client is closed; create a new WirefanClient"),
      );
    }
    if (this.#state === "connected") return Promise.resolve();
    const p = new Promise<void>((resolve, reject) => {
      this.#connectWaiters.push({ resolve, reject });
    });
    if (this.#state === "idle") {
      this.#setState("connecting");
      this.#dial();
    }
    return p;
  }

  /**
   * Permanently close the client. No reconnect will ever follow. In-flight
   * operations reject with ConnectionClosedError. The instance cannot be
   * reused; construct a new client to connect again.
   */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    if (this.#reconnectTimer !== null) {
      clearTimeout(this.#reconnectTimer);
      this.#reconnectTimer = null;
    }
    const ws = this.#ws;
    this.#ws = null;
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      try {
        ws.close(1000, "client-close");
      } catch {
        // Closing an already-failed socket may throw; the socket is gone either way.
      }
    }
    this.#failPending(new ConnectionClosedError("client closed"));
    this.#rejectConnectWaiters(new ConnectionClosedError("client closed"));
    this.#socketId = null;
    for (const rec of this.#channels.values()) rec.handlers.clear();
    this.#channels.clear();
    this.#setState("closed");
    this.#emit("closed", { reason: "explicit" });
  }

  #dial(): void {
    let ws: WebSocketLike;
    try {
      ws = new this.#WS(this.#url);
    } catch (e) {
      this.#onDrop(undefined, e instanceof Error ? e.message : String(e));
      return;
    }
    this.#ws = ws;
    ws.onmessage = (ev) => this.#onMessage(ev.data);
    ws.onclose = (ev) => {
      if (this.#ws !== ws) return;
      this.#onDrop(ev.code, ev.reason);
    };
    ws.onerror = () => {
      // Some implementations fire error without close on dial failure.
      // The close handler is the single drop path; force it if needed.
      if (this.#ws !== ws) return;
      if (ws.readyState !== WS_OPEN) {
        try {
          ws.close();
        } catch {
          // Already closed.
        }
      }
    };
    // The connection is not usable until the `connected` frame (§3.3);
    // onopen is intentionally not treated as "ready".
    ws.onopen = null;
  }

  #onMessage(raw: unknown): void {
    if (typeof raw !== "string") return; // protocol is text frames only (§2)
    let frame: ServerFrame;
    try {
      frame = JSON.parse(raw) as ServerFrame;
    } catch {
      return; // unparseable server frame; ignore
    }
    switch (frame.type) {
      case "connected":
        this.#onConnected(frame);
        return;
      case "subscribed":
        this.#settleOp("subscribe", frame.channel);
        return;
      case "unsubscribed":
        this.#settleOp("unsubscribe", frame.channel);
        return;
      case "event":
        this.#onEvent(frame);
        return;
      case "error":
        this.#onErrorFrame(frame);
        return;
      default:
        // Unknown frame types must be ignored (§14 forward-compatibility).
        return;
    }
  }

  #onConnected(frame: ConnectedFrame): void {
    this.#socketId = frame.socket_id;
    const reconnected = this.#attempt > 0;
    this.#attempt = 0;
    this.#setState("connected");
    if (frame.version !== "v1") {
      this.#emit(
        "error",
        new Error(`server speaks protocol "${frame.version}", client expects "v1"`),
      );
    }
    const waiters = this.#connectWaiters;
    this.#connectWaiters = [];
    for (const w of waiters) w.resolve();
    this.#emit("connected", { socketId: frame.socket_id, reconnected });
    if (reconnected) void this.#resubscribeAll();
  }

  #onEvent(frame: EventFrame): void {
    const rec = this.#channels.get(frame.channel);
    if (!rec) return;
    const ev: ChannelEvent = {
      channel: frame.channel,
      data: frame.data,
      id: frame.id,
    };
    for (const h of [...rec.handlers]) {
      try {
        h(ev);
      } catch {
        // A throwing handler must not break dispatch to the others.
      }
    }
  }

  #onErrorFrame(frame: ErrorFrame): void {
    const err = new WirefanError(frame.code, frame.message);
    // Error frames carry no channel (§5.9). The server handles inbound frames
    // sequentially, so an error answering a control op arrives before any
    // later op's ack: attribute by code class to the oldest matching pending op.
    if (SUBSCRIBE_ERROR_CODES.has(frame.code)) {
      const idx = this.#pending.findIndex((op) => op.kind === "subscribe");
      if (idx !== -1) {
        const op = this.#pending.splice(idx, 1)[0]!;
        clearTimeout(op.timer);
        // The subscribe failed: forget the channel unless it was already
        // confirmed on this connection (idempotent re-subscribes).
        const rec = this.#channels.get(op.channel);
        if (rec && !rec.confirmed) this.#channels.delete(op.channel);
        op.reject(err);
        return;
      }
    }
    this.#emit("error", err);
  }

  #settleOp(kind: PendingOp["kind"], channel: string): void {
    const idx = this.#pending.findIndex(
      (op) => op.kind === kind && op.channel === channel,
    );
    if (idx !== -1) {
      const op = this.#pending.splice(idx, 1)[0]!;
      clearTimeout(op.timer);
      op.resolve();
    }
    if (kind === "subscribe") {
      const rec = this.#channels.get(channel);
      if (rec) rec.confirmed = true;
    }
  }

  #failPending(err: Error): void {
    const pending = this.#pending;
    this.#pending = [];
    for (const op of pending) {
      clearTimeout(op.timer);
      op.reject(err);
    }
  }

  #rejectConnectWaiters(err: Error): void {
    const waiters = this.#connectWaiters;
    this.#connectWaiters = [];
    for (const w of waiters) w.reject(err);
  }

  // ----- reconnect ---------------------------------------------------------

  #onDrop(code?: number, reason?: string): void {
    if (this.#closed) return;
    const ws = this.#ws;
    this.#ws = null;
    if (ws) ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    this.#socketId = null;
    this.#failPending(
      new ConnectionClosedError("connection dropped", code, reason),
    );
    for (const rec of this.#channels.values()) rec.confirmed = false;

    const canRetry =
      this.#reconnect !== false && this.#attempt + 1 <= this.#reconnect.maxAttempts;
    this.#emit("disconnected", { willReconnect: canRetry, ...(code !== undefined ? { code } : {}), ...(reason ? { reason } : {}) });

    if (!canRetry) {
      this.#closed = true;
      this.#rejectConnectWaiters(
        new ConnectionClosedError("reconnect attempts exhausted", code, reason),
      );
      for (const rec of this.#channels.values()) rec.handlers.clear();
      this.#channels.clear();
      this.#setState("closed");
      this.#emit("closed", { reason: "exhausted" });
      return;
    }

    const r = this.#reconnect as Required<ReconnectOptions>;
    this.#attempt += 1;
    const base = Math.min(
      r.maxDelayMs,
      r.initialDelayMs * Math.pow(r.multiplier, this.#attempt - 1),
    );
    const jittered = Math.round(base * (1 + r.jitter * (2 * this.#random() - 1)));
    const delayMs = Math.max(0, jittered);
    this.#setState("reconnecting");
    this.#emit("reconnecting", { attempt: this.#attempt, delayMs });
    this.#reconnectTimer = setTimeout(() => {
      this.#reconnectTimer = null;
      if (this.#closed) return;
      this.#dial();
    }, delayMs);
  }

  /**
   * After a reconnect, restore every channel the caller had. Tokens for
   * private-/presence- channels are re-fetched: the old ones were bound to
   * the previous socket_id and are single-use besides.
   */
  async #resubscribeAll(): Promise<void> {
    const channels = [...this.#channels.keys()];
    const restored: string[] = [];
    for (const channel of channels) {
      if (this.#state !== "connected") return; // dropped again mid-restore
      const rec = this.#channels.get(channel);
      if (!rec) continue; // unsubscribed meanwhile
      try {
        await this.#ensureSubscribed(channel, rec);
        restored.push(channel);
      } catch (e) {
        // Surface the failure and drop the dead subscription rather than
        // silently pretending it is alive.
        this.#channels.delete(channel);
        this.#emit(
          "error",
          e instanceof Error
            ? e
            : new Error(`resubscribe ${channel} failed: ${String(e)}`),
        );
      }
    }
    if (this.#state === "connected" && restored.length > 0) {
      this.#emit("resubscribed", { channels: restored });
    }
  }

  // ----- operations --------------------------------------------------------

  /**
   * Subscribe to a channel. Resolves with a Subscription handle once the
   * server acks. `private-` / `presence-` channels require an `authorize`
   * callback in the client options.
   *
   * Subscribing to a channel that is already subscribed reuses the server
   * subscription and just attaches the handler.
   */
  async subscribe(
    channel: string,
    handler?: (ev: ChannelEvent) => void,
  ): Promise<Subscription> {
    if (this.#closed) throw new ConnectionClosedError("client is closed");
    if (this.#state !== "connected") {
      throw new ConnectionClosedError(
        `cannot subscribe while ${this.#state}; await connect() first`,
      );
    }
    if (needsToken(channel) && !this.#authorize) {
      throw new ConfigurationError(
        `channel "${channel}" requires a token: pass an authorize() callback in WirefanClientOptions`,
      );
    }

    let rec = this.#channels.get(channel);
    const alreadyConfirmed = rec?.confirmed === true;
    if (!rec) {
      rec = { handlers: new Set(), confirmed: false, inflight: null };
      this.#channels.set(channel, rec);
    }
    if (handler) rec.handlers.add(handler);

    if (!alreadyConfirmed) {
      try {
        await this.#ensureSubscribed(channel, rec);
      } catch (e) {
        if (handler) rec.handlers.delete(handler);
        const cur = this.#channels.get(channel);
        if (cur && !cur.confirmed && cur.handlers.size === 0) {
          this.#channels.delete(channel);
        }
        throw e;
      }
    }

    let active = true;
    const client = this;
    return {
      channel,
      get active() {
        return active && client.#channels.has(channel);
      },
      async unsubscribe(): Promise<void> {
        if (!active) return;
        active = false;
        const cur = client.#channels.get(channel);
        if (!cur) return;
        if (handler) cur.handlers.delete(handler);
        if (cur.handlers.size > 0) return; // other handles still want it
        client.#channels.delete(channel);
        if (client.#state !== "connected") return; // nothing to tell the server
        await client.#sendOp("unsubscribe", channel, { type: "unsubscribe", channel });
      },
    };
  }

  /**
   * Publish `data` to a channel. The caller must be subscribed (server rule:
   * publish requires a prior subscribe, §5.7). Resolves once the frame is
   * handed to the socket; delivery is at-most-once and publish has no ack.
   * A server-side rejection (NOT_SUBSCRIBED, RATE_LIMITED, ...) surfaces on
   * the client's "error" event.
   */
  publish(channel: string, data: unknown): void {
    if (this.#state !== "connected" || !this.#ws || this.#ws.readyState !== WS_OPEN) {
      throw new ConnectionClosedError(
        `cannot publish while ${this.#state}`,
      );
    }
    this.#ws.send(JSON.stringify({ type: "publish", channel, data }));
  }

  /** Concurrent subscribes to the same channel share one wire attempt. */
  #ensureSubscribed(channel: string, rec: ChannelRecord): Promise<void> {
    if (rec.confirmed) return Promise.resolve();
    if (!rec.inflight) {
      rec.inflight = this.#sendSubscribe(channel).finally(() => {
        const cur = this.#channels.get(channel);
        if (cur) cur.inflight = null;
      });
    }
    return rec.inflight;
  }

  async #sendSubscribe(channel: string): Promise<void> {
    const frame: { type: "subscribe"; channel: string; token?: string } = {
      type: "subscribe",
      channel,
    };
    if (needsToken(channel)) {
      const socketId = this.#socketId;
      if (!socketId) throw new ConnectionClosedError("not connected");
      // Re-fetched every time: tokens are single-use and socket-bound.
      frame.token = await this.#authorize!({ socketId, channel });
      if (this.#state !== "connected") {
        throw new ConnectionClosedError("connection dropped while authorizing");
      }
    }
    await this.#sendOp("subscribe", channel, frame);
  }

  #sendOp(
    kind: PendingOp["kind"],
    channel: string,
    frame: object,
  ): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const ws = this.#ws;
      if (!ws || ws.readyState !== WS_OPEN) {
        reject(new ConnectionClosedError("not connected"));
        return;
      }
      const op: PendingOp = {
        kind,
        channel,
        resolve,
        reject,
        timer: setTimeout(() => {
          const idx = this.#pending.indexOf(op);
          if (idx !== -1) this.#pending.splice(idx, 1);
          reject(new AckTimeoutError(kind, channel, this.#ackTimeoutMs));
        }, this.#ackTimeoutMs),
      };
      this.#pending.push(op);
      try {
        ws.send(JSON.stringify(frame));
      } catch (e) {
        const idx = this.#pending.indexOf(op);
        if (idx !== -1) this.#pending.splice(idx, 1);
        clearTimeout(op.timer);
        reject(
          e instanceof Error ? e : new ConnectionClosedError("send failed"),
        );
      }
    });
  }
}
