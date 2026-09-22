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
  /**
   * The client frame type this error answers. Optional: older servers omit
   * it, and BAD_JSON / BAD_TYPE never carry it.
   */
  op?: "subscribe" | "unsubscribe" | "publish" | (string & {});
  /**
   * The channel exactly as the client sent it in that frame. Optional:
   * omitted by older servers and when the frame had none.
   */
  channel?: string;
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
  /** The frame type the error answers, when the server names it. */
  readonly op: string | undefined;
  /** The channel of the frame the error answers, when the server names it. */
  readonly channel: string | undefined;
  constructor(
    code: WirefanErrorCode,
    message: string,
    detail: { op?: string; channel?: string } = {},
  ) {
    super(`${code}: ${message}`);
    this.name = "WirefanError";
    this.code = code;
    this.op = detail.op;
    this.channel = detail.channel;
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
  /**
   * How long a dial may take to produce the server's `connected` frame before
   * the attempt is abandoned and fed to the normal reconnect path. Bounds the
   * whole handshake (TCP + upgrade + first frame), so a load balancer that
   * completes the 101 upgrade but never reaches a backend cannot wedge the
   * client in "connecting" forever. Default 10 000 ms.
   */
  handshakeTimeoutMs?: number;
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
  /**
   * Connection is ready (fired on first connect and on every reconnect).
   * `reconnected` is false for the client's first successful connection,
   * however many dials that took, and true for every one after it.
   */
  connected: { socketId: string; reconnected: boolean };
  /** The transport dropped. `willReconnect` says whether a retry is scheduled. */
  disconnected: { code?: number; reason?: string; willReconnect: boolean };
  /** A reconnect attempt is scheduled. */
  reconnecting: { attempt: number; delayMs: number };
  /**
   * Channels restored after a reconnect: one event for the first pass over
   * every channel, then one per channel whose resubscribe needed a retry.
   */
  resubscribed: { channels: string[] };
  /**
   * A server error frame that does not belong to an in-flight operation,
   * or a resubscribe failure after reconnect (each transient failure that
   * will be retried, and the definitive refusal that drops a channel).
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
  /** Pending backoff retry of a transiently failed resubscribe. */
  retryTimer: ReturnType<typeof setTimeout> | null;
}

/**
 * Error codes that answer a `subscribe` request on servers that predate the
 * error frame's `op` / `channel` fields. Those frames name no operation, so
 * attribution falls back to code class + FIFO order: the server processes
 * one inbound frame at a time, so replies to control ops arrive in the order
 * the ops were sent. The heuristic can still blame a subscribe for a publish
 * or unsubscribe rejection that shares a code; servers that send `op` and
 * `channel` are routed exactly and never reach this set.
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

/**
 * Refusals that retrying cannot fix (plus every LIMIT_* code): a resubscribe
 * that ends in one of these drops the channel. Every other failure (ack
 * timeout, RATE_LIMITED, RATE_LIMITED_CONN, an authorize() callback that
 * throws, a code this client does not know) is treated as transient and
 * retried with backoff, so a reconnect herd cannot silently strip channels.
 */
const DEFINITIVE_SUBSCRIBE_CODES = new Set<string>([
  "AUTH_FAILED",
  "AUTH_REPLAYED",
  "RESERVED_CHANNEL",
  "BAD_CHANNEL",
  "SUBSCRIBE_FAILED",
]);

function isDefinitiveRefusal(err: Error): boolean {
  if (!(err instanceof WirefanError)) return false;
  return DEFINITIVE_SUBSCRIBE_CODES.has(err.code) || err.code.startsWith("LIMIT_");
}

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
  readonly #handshakeTimeoutMs: number;
  readonly #random: () => number;

  #ws: WebSocketLike | null = null;
  #state: WirefanState = "idle";
  #socketId: string | null = null;
  #closed = false;
  /**
   * Set by the first `connected` frame. The retry counter cannot answer
   * "is this a reconnect?": it is also non-zero when the very first dial
   * failed and a later attempt is the first to succeed.
   */
  #everConnected = false;
  #attempt = 0;
  #reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  #handshakeTimer: ReturnType<typeof setTimeout> | null = null;
  /**
   * Bumped on every `connected` frame. Async work that spans an await (the
   * `authorize()` round trip) captures the epoch first and re-checks it after,
   * so a token minted against one connection can never be sent on a later one.
   */
  #epoch = 0;

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
    this.#handshakeTimeoutMs = options.handshakeTimeoutMs ?? 10_000;
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
    this.#clearHandshakeTimer();
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
    for (const rec of this.#channels.values()) {
      this.#cancelRetry(rec);
      rec.handlers.clear();
    }
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
    // Bound the whole handshake. Without this, an upgrade that succeeds and
    // then goes silent (no `connected` frame, no close) would strand the
    // client in "connecting" with no event ever firing.
    this.#handshakeTimer = setTimeout(() => {
      this.#handshakeTimer = null;
      if (this.#ws !== ws || this.#state === "connected") return;
      // Detach handlers first so ws.close() cannot re-enter #onDrop; then
      // route through #onDrop once so backoff/maxAttempts work unchanged.
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      try {
        ws.close(4000, "handshake-timeout");
      } catch {
        // Closing a not-yet-open socket may throw; it is abandoned either way.
      }
      this.#onDrop(undefined, "handshake timeout");
    }, this.#handshakeTimeoutMs);
  }

  #clearHandshakeTimer(): void {
    if (this.#handshakeTimer !== null) {
      clearTimeout(this.#handshakeTimer);
      this.#handshakeTimer = null;
    }
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
    this.#clearHandshakeTimer();
    this.#epoch += 1;
    this.#socketId = frame.socket_id;
    const reconnected = this.#everConnected;
    this.#everConnected = true;
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
    const detail: { op?: string; channel?: string } = {};
    if (typeof frame.op === "string") detail.op = frame.op;
    if (typeof frame.channel === "string") detail.channel = frame.channel;
    const err = new WirefanError(frame.code, frame.message, detail);
    const idx = this.#pendingIndexFor(frame);
    if (idx === -1) {
      this.#emit("error", err);
      return;
    }
    const op = this.#pending.splice(idx, 1)[0]!;
    clearTimeout(op.timer);
    if (op.kind === "subscribe" && isDefinitiveRefusal(err)) {
      // Drop a refused channel before anyone awaiting it resumes. Otherwise
      // an interrupted subscribe() could see the record between this attempt
      // settling and its owner reacting, and adopt it with a fresh attempt.
      // Transient refusals leave the record to the awaiting caller:
      // subscribe() forgets a channel nobody else holds, while a resubscribe
      // keeps it and retries (#resubscribe).
      const rec = this.#channels.get(op.channel);
      if (rec && !rec.confirmed) {
        this.#cancelRetry(rec);
        this.#channels.delete(op.channel);
      }
    }
    op.reject(err);
  }

  /** The index in #pending of the operation an error frame answers, or -1. */
  #pendingIndexFor(frame: ErrorFrame): number {
    if (frame.op !== undefined || frame.channel !== undefined) {
      // The server named the frame it answers: route exactly, never guess.
      // Publish has no pending op, so its errors go to the error event.
      if (frame.op !== "subscribe" && frame.op !== "unsubscribe") return -1;
      // A missing channel means the client sent none (only "" can do that).
      const channel = frame.channel ?? "";
      return this.#pending.findIndex(
        (op) => op.kind === frame.op && op.channel === channel,
      );
    }
    // Older server: the frame names no operation. It handles inbound frames
    // sequentially, so an error answering a control op arrives before any
    // later op's ack: attribute by code class to the oldest pending subscribe.
    if (!SUBSCRIBE_ERROR_CODES.has(frame.code)) return -1;
    return this.#pending.findIndex((op) => op.kind === "subscribe");
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
      if (rec) {
        rec.confirmed = true;
      } else {
        // Nobody wants this channel any more (the subscribe timed out or was
        // abandoned before its ack landed), but the server now holds it and
        // would keep fanning events to a record that no longer exists. Undo
        // it. Fire-and-forget: the unsubscribed ack matches no pending op.
        this.#sendRaw({ type: "unsubscribe", channel });
      }
    }
  }

  /** Best-effort send with no ack tracking. */
  #sendRaw(frame: object): void {
    const ws = this.#ws;
    if (!ws || ws.readyState !== WS_OPEN) return;
    try {
      ws.send(JSON.stringify(frame));
    } catch {
      // The socket is failing; its close event drives the drop path.
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
    this.#clearHandshakeTimer();
    const ws = this.#ws;
    this.#ws = null;
    if (ws) ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    this.#socketId = null;
    this.#failPending(
      new ConnectionClosedError("connection dropped", code, reason),
    );
    // Reset per-connection subscription state. Clearing `inflight` matters:
    // a subscribe attempt stuck awaiting authorize() when the drop hit is now
    // stale (its token targets the dead socket), and the post-reconnect
    // resubscribe must start a fresh attempt, not adopt the old one. A
    // pending resubscribe retry is moot too: the next #onConnected restores
    // every record from scratch.
    for (const rec of this.#channels.values()) {
      rec.confirmed = false;
      rec.inflight = null;
      this.#cancelRetry(rec);
    }

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

    this.#attempt += 1;
    const delayMs = this.#backoffDelay(this.#attempt);
    this.#setState("reconnecting");
    this.#emit("reconnecting", { attempt: this.#attempt, delayMs });
    this.#reconnectTimer = setTimeout(() => {
      this.#reconnectTimer = null;
      if (this.#closed) return;
      this.#dial();
    }, delayMs);
  }

  /** Exponential backoff with jitter for the given 1-based attempt. */
  #backoffDelay(attempt: number): number {
    const r = this.#reconnect === false ? DEFAULT_RECONNECT : this.#reconnect;
    const base = Math.min(
      r.maxDelayMs,
      r.initialDelayMs * Math.pow(r.multiplier, attempt - 1),
    );
    const jittered = Math.round(base * (1 + r.jitter * (2 * this.#random() - 1)));
    return Math.max(0, jittered);
  }

  #cancelRetry(rec: ChannelRecord): void {
    if (rec.retryTimer !== null) {
      clearTimeout(rec.retryTimer);
      rec.retryTimer = null;
    }
  }

  /**
   * After a reconnect, restore every channel the caller had. Tokens for
   * private-/presence- channels are re-fetched: the old ones were bound to
   * the previous socket_id and are single-use besides.
   */
  async #resubscribeAll(): Promise<void> {
    const epoch = this.#epoch;
    const channels = [...this.#channels.keys()];
    const restored: string[] = [];
    for (const channel of channels) {
      // Dropped again mid-restore; after a reconnect, that connection's own
      // #resubscribeAll takes over.
      if (this.#epoch !== epoch || this.#state !== "connected") return;
      const rec = this.#channels.get(channel);
      if (!rec) continue; // unsubscribed meanwhile
      if (await this.#resubscribe(channel, rec, epoch, 0)) restored.push(channel);
    }
    if (this.#epoch === epoch && this.#state === "connected" && restored.length > 0) {
      this.#emit("resubscribed", { channels: restored });
    }
  }

  /**
   * One resubscribe attempt for a channel the caller still holds; true once
   * it is confirmed. A failure decides the record's fate by class:
   * - ConnectionClosedError: the connection dropped again. Keep the record
   *   without an error event; the next #onConnected restores it.
   * - A definitive refusal (isDefinitiveRefusal): surface it. #onErrorFrame
   *   has already dropped the channel rather than pretend it is alive.
   * - Anything else is transient: surface it, keep the record, and retry
   *   on this connection with the reconnect backoff curve.
   */
  async #resubscribe(
    channel: string,
    rec: ChannelRecord,
    epoch: number,
    retries: number,
  ): Promise<boolean> {
    try {
      await this.#ensureSubscribed(channel, rec);
      return true;
    } catch (e) {
      if (e instanceof ConnectionClosedError) return false;
      const err =
        e instanceof Error ? e : new Error(`resubscribe ${channel} failed: ${String(e)}`);
      this.#emit("error", err);
      if (isDefinitiveRefusal(err)) return false;
      if (this.#channels.get(channel) !== rec) return false; // unsubscribed meanwhile
      if (this.#epoch === epoch && this.#state === "connected") {
        this.#scheduleResubscribe(channel, rec, epoch, retries + 1);
      }
      return false;
    }
  }

  /**
   * Retry a transiently failed resubscribe after a backoff. Bound to the
   * connection it was scheduled on: a drop cancels it (#onDrop), and so does
   * the caller unsubscribing or closing the client.
   */
  #scheduleResubscribe(
    channel: string,
    rec: ChannelRecord,
    epoch: number,
    retries: number,
  ): void {
    this.#cancelRetry(rec);
    rec.retryTimer = setTimeout(() => {
      rec.retryTimer = null;
      if (this.#epoch !== epoch || this.#state !== "connected") return;
      if (this.#channels.get(channel) !== rec) return;
      void this.#resubscribe(channel, rec, epoch, retries).then((ok) => {
        if (ok && this.#epoch === epoch && this.#state === "connected") {
          this.#emit("resubscribed", { channels: [channel] });
        }
      });
    }, this.#backoffDelay(retries));
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
      rec = { handlers: new Set(), confirmed: false, inflight: null, retryTimer: null };
      this.#channels.set(channel, rec);
    }
    if (handler) rec.handlers.add(handler);

    if (!alreadyConfirmed) {
      let epoch = this.#epoch;
      for (;;) {
        try {
          await this.#ensureSubscribed(channel, rec);
          break;
        } catch (e) {
          // A drop can cut this attempt short (typically mid-authorize()),
          // and by the time it fails the client may have reconnected and its
          // resubscribe taken the channel over, since the record still holds
          // this handler. Adopt that attempt (already confirmed, still in
          // flight, or a fresh one) rather than reject while the channel
          // lives on without the caller's handler. Each pass needs a newer
          // connection, which bounds the loop.
          if (
            e instanceof ConnectionClosedError &&
            this.#state === "connected" &&
            this.#epoch !== epoch &&
            this.#channels.get(channel) === rec
          ) {
            epoch = this.#epoch;
            continue;
          }
          if (handler) rec.handlers.delete(handler);
          const cur = this.#channels.get(channel);
          if (cur && !cur.confirmed && cur.handlers.size === 0) {
            this.#cancelRetry(cur);
            this.#channels.delete(channel);
          }
          throw e;
        }
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
        client.#cancelRetry(cur);
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
      const attempt: Promise<void> = this.#sendSubscribe(channel).finally(() => {
        // Only clear our own attempt: #onDrop may already have nulled the
        // slot and a post-reconnect resubscribe may have installed a new one.
        const cur = this.#channels.get(channel);
        if (cur && cur.inflight === attempt) cur.inflight = null;
      });
      rec.inflight = attempt;
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
      const epoch = this.#epoch;
      // Re-fetched every time: tokens are single-use and socket-bound.
      frame.token = await this.#authorize!({ socketId, channel });
      // The authorize() round trip can span a drop, or a drop AND a
      // reconnect. Checking #state alone is not enough (after a reconnect
      // the state is "connected" again on a different socket), so require
      // the same connection epoch, or the token targets a dead socket_id.
      if (this.#state !== "connected" || this.#epoch !== epoch) {
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
