// VENDORED BUILD, do not edit by hand.
// Source: clients/js/src/index.ts. Regenerate with:
//   cd clients/js && npm run vendor:web
/**
 * @wirefan/client: JavaScript client for the wirefan WebSocket fan-out server.
 *
 * Wire protocol: docs/PROTOCOL.md (protocol id "v1").
 *
 * Single-file on purpose: the compiled output is one dependency-free ESM
 * module, which lets the repo's demo page vendor it without a bundler.
 */
var __classPrivateFieldSet = (this && this.__classPrivateFieldSet) || function (receiver, state, value, kind, f) {
    if (kind === "m") throw new TypeError("Private method is not writable");
    if (kind === "a" && !f) throw new TypeError("Private accessor was defined without a setter");
    if (typeof state === "function" ? receiver !== state || !f : !state.has(receiver)) throw new TypeError("Cannot write private member to an object whose class did not declare it");
    return (kind === "a" ? f.call(receiver, value) : f ? f.value = value : state.set(receiver, value)), value;
};
var __classPrivateFieldGet = (this && this.__classPrivateFieldGet) || function (receiver, state, kind, f) {
    if (kind === "a" && !f) throw new TypeError("Private accessor was defined without a getter");
    if (typeof state === "function" ? receiver !== state || !f : !state.has(receiver)) throw new TypeError("Cannot read private member from an object whose class did not declare it");
    return kind === "m" ? f : kind === "a" ? f.call(receiver) : f ? f.value : state.get(receiver);
};
var _WirefanClient_instances, _WirefanClient_url, _WirefanClient_authorize, _WirefanClient_WS, _WirefanClient_reconnect, _WirefanClient_ackTimeoutMs, _WirefanClient_random, _WirefanClient_ws, _WirefanClient_state, _WirefanClient_socketId, _WirefanClient_closed, _WirefanClient_attempt, _WirefanClient_reconnectTimer, _WirefanClient_channels, _WirefanClient_pending, _WirefanClient_listeners, _WirefanClient_connectWaiters, _WirefanClient_emit, _WirefanClient_setState, _WirefanClient_dial, _WirefanClient_onMessage, _WirefanClient_onConnected, _WirefanClient_onEvent, _WirefanClient_onErrorFrame, _WirefanClient_settleOp, _WirefanClient_failPending, _WirefanClient_rejectConnectWaiters, _WirefanClient_onDrop, _WirefanClient_resubscribeAll, _WirefanClient_ensureSubscribed, _WirefanClient_sendSubscribe, _WirefanClient_sendOp;
// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------
/** A server `error` frame surfaced as a typed exception. */
export class WirefanError extends Error {
    constructor(code, message) {
        super(`${code}: ${message}`);
        this.name = "WirefanError";
        this.code = code;
    }
}
/** The operation could not complete because the connection dropped or closed. */
export class ConnectionClosedError extends Error {
    constructor(message, closeCode, reason) {
        super(message);
        this.name = "ConnectionClosedError";
        this.closeCode = closeCode;
        this.reason = reason;
    }
}
/** A subscribe/unsubscribe acknowledgement did not arrive in time. */
export class AckTimeoutError extends Error {
    constructor(op, channel, ms) {
        super(`${op} "${channel}" not acknowledged within ${ms}ms`);
        this.name = "AckTimeoutError";
    }
}
/** The client was used in a way its configuration cannot support. */
export class ConfigurationError extends Error {
    constructor(message) {
        super(message);
        this.name = "ConfigurationError";
    }
}
const WS_OPEN = 1;
/**
 * Error codes that answer a `subscribe` request. The server's error frames
 * carry no channel field (§5.9), so attribution is by code class + FIFO order:
 * the server processes one inbound frame at a time, so replies to control ops
 * arrive in the order the ops were sent.
 */
const SUBSCRIBE_ERROR_CODES = new Set([
    "AUTH_FAILED",
    "AUTH_REPLAYED",
    "BAD_CHANNEL",
    "RESERVED_CHANNEL",
    "LIMIT_CHANNELS",
    "LIMIT_SUBSCRIBERS",
    "SUBSCRIBE_FAILED",
    "RATE_LIMITED",
]);
const DEFAULT_RECONNECT = {
    initialDelayMs: 300,
    maxDelayMs: 15000,
    multiplier: 2,
    jitter: 0.25,
    maxAttempts: Number.POSITIVE_INFINITY,
};
function needsToken(channel) {
    return channel.startsWith("private-") || channel.startsWith("presence-");
}
function buildUrl(raw, key) {
    let url = raw;
    if (url.startsWith("https://"))
        url = "wss://" + url.slice(8);
    else if (url.startsWith("http://"))
        url = "ws://" + url.slice(7);
    const u = new URL(url);
    if (u.pathname === "/" || u.pathname === "")
        u.pathname = "/v1/connect";
    u.searchParams.set("key", key);
    return u.toString();
}
// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------
export class WirefanClient {
    constructor(options) {
        _WirefanClient_instances.add(this);
        _WirefanClient_url.set(this, void 0);
        _WirefanClient_authorize.set(this, void 0);
        _WirefanClient_WS.set(this, void 0);
        _WirefanClient_reconnect.set(this, void 0);
        _WirefanClient_ackTimeoutMs.set(this, void 0);
        _WirefanClient_random.set(this, void 0);
        _WirefanClient_ws.set(this, null);
        _WirefanClient_state.set(this, "idle");
        _WirefanClient_socketId.set(this, null);
        _WirefanClient_closed.set(this, false);
        _WirefanClient_attempt.set(this, 0);
        _WirefanClient_reconnectTimer.set(this, null);
        _WirefanClient_channels.set(this, new Map());
        _WirefanClient_pending.set(this, []);
        _WirefanClient_listeners.set(this, new Map());
        _WirefanClient_connectWaiters.set(this, []);
        if (!options.url)
            throw new ConfigurationError("url is required");
        if (!options.key)
            throw new ConfigurationError("key is required");
        __classPrivateFieldSet(this, _WirefanClient_url, buildUrl(options.url, options.key), "f");
        __classPrivateFieldSet(this, _WirefanClient_authorize, options.authorize, "f");
        const WS = options.webSocket ??
            globalThis.WebSocket;
        if (!WS) {
            throw new ConfigurationError("no WebSocket implementation: pass options.webSocket (e.g. from the 'ws' package) on platforms without a global WebSocket");
        }
        __classPrivateFieldSet(this, _WirefanClient_WS, WS, "f");
        __classPrivateFieldSet(this, _WirefanClient_reconnect, options.reconnect === false
            ? false
            : { ...DEFAULT_RECONNECT, ...(options.reconnect ?? {}) }, "f");
        __classPrivateFieldSet(this, _WirefanClient_ackTimeoutMs, options.ackTimeoutMs ?? 10000, "f");
        __classPrivateFieldSet(this, _WirefanClient_random, options.random ?? Math.random, "f");
    }
    /** Current lifecycle state. */
    get state() {
        return __classPrivateFieldGet(this, _WirefanClient_state, "f");
    }
    /** The server-issued socket id, or null while not connected. */
    get socketId() {
        return __classPrivateFieldGet(this, _WirefanClient_socketId, "f");
    }
    // ----- events ------------------------------------------------------------
    /** Register a listener. Returns a function that removes it. */
    on(event, handler) {
        let set = __classPrivateFieldGet(this, _WirefanClient_listeners, "f").get(event);
        if (!set) {
            set = new Set();
            __classPrivateFieldGet(this, _WirefanClient_listeners, "f").set(event, set);
        }
        set.add(handler);
        return () => this.off(event, handler);
    }
    off(event, handler) {
        __classPrivateFieldGet(this, _WirefanClient_listeners, "f").get(event)?.delete(handler);
    }
    // ----- lifecycle ---------------------------------------------------------
    /**
     * Open the connection. Resolves once the server's `connected` frame arrives.
     * With reconnect enabled the promise stays pending across failed attempts
     * and rejects only when retries are exhausted or the client is closed.
     */
    connect() {
        if (__classPrivateFieldGet(this, _WirefanClient_closed, "f")) {
            return Promise.reject(new ConnectionClosedError("client is closed; create a new WirefanClient"));
        }
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") === "connected")
            return Promise.resolve();
        const p = new Promise((resolve, reject) => {
            __classPrivateFieldGet(this, _WirefanClient_connectWaiters, "f").push({ resolve, reject });
        });
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") === "idle") {
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_setState).call(this, "connecting");
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_dial).call(this);
        }
        return p;
    }
    /**
     * Permanently close the client. No reconnect will ever follow. In-flight
     * operations reject with ConnectionClosedError. The instance cannot be
     * reused; construct a new client to connect again.
     */
    close() {
        if (__classPrivateFieldGet(this, _WirefanClient_closed, "f"))
            return;
        __classPrivateFieldSet(this, _WirefanClient_closed, true, "f");
        if (__classPrivateFieldGet(this, _WirefanClient_reconnectTimer, "f") !== null) {
            clearTimeout(__classPrivateFieldGet(this, _WirefanClient_reconnectTimer, "f"));
            __classPrivateFieldSet(this, _WirefanClient_reconnectTimer, null, "f");
        }
        const ws = __classPrivateFieldGet(this, _WirefanClient_ws, "f");
        __classPrivateFieldSet(this, _WirefanClient_ws, null, "f");
        if (ws) {
            ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
            try {
                ws.close(1000, "client-close");
            }
            catch {
                // Closing an already-failed socket may throw; the socket is gone either way.
            }
        }
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_failPending).call(this, new ConnectionClosedError("client closed"));
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_rejectConnectWaiters).call(this, new ConnectionClosedError("client closed"));
        __classPrivateFieldSet(this, _WirefanClient_socketId, null, "f");
        for (const rec of __classPrivateFieldGet(this, _WirefanClient_channels, "f").values())
            rec.handlers.clear();
        __classPrivateFieldGet(this, _WirefanClient_channels, "f").clear();
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_setState).call(this, "closed");
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "closed", { reason: "explicit" });
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
    async subscribe(channel, handler) {
        if (__classPrivateFieldGet(this, _WirefanClient_closed, "f"))
            throw new ConnectionClosedError("client is closed");
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") !== "connected") {
            throw new ConnectionClosedError(`cannot subscribe while ${__classPrivateFieldGet(this, _WirefanClient_state, "f")}; await connect() first`);
        }
        if (needsToken(channel) && !__classPrivateFieldGet(this, _WirefanClient_authorize, "f")) {
            throw new ConfigurationError(`channel "${channel}" requires a token: pass an authorize() callback in WirefanClientOptions`);
        }
        let rec = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(channel);
        const alreadyConfirmed = rec?.confirmed === true;
        if (!rec) {
            rec = { handlers: new Set(), confirmed: false, inflight: null };
            __classPrivateFieldGet(this, _WirefanClient_channels, "f").set(channel, rec);
        }
        if (handler)
            rec.handlers.add(handler);
        if (!alreadyConfirmed) {
            try {
                await __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_ensureSubscribed).call(this, channel, rec);
            }
            catch (e) {
                if (handler)
                    rec.handlers.delete(handler);
                const cur = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(channel);
                if (cur && !cur.confirmed && cur.handlers.size === 0) {
                    __classPrivateFieldGet(this, _WirefanClient_channels, "f").delete(channel);
                }
                throw e;
            }
        }
        let active = true;
        const client = this;
        return {
            channel,
            get active() {
                return active && __classPrivateFieldGet(client, _WirefanClient_channels, "f").has(channel);
            },
            async unsubscribe() {
                if (!active)
                    return;
                active = false;
                const cur = __classPrivateFieldGet(client, _WirefanClient_channels, "f").get(channel);
                if (!cur)
                    return;
                if (handler)
                    cur.handlers.delete(handler);
                if (cur.handlers.size > 0)
                    return; // other handles still want it
                __classPrivateFieldGet(client, _WirefanClient_channels, "f").delete(channel);
                if (__classPrivateFieldGet(client, _WirefanClient_state, "f") !== "connected")
                    return; // nothing to tell the server
                await __classPrivateFieldGet(client, _WirefanClient_instances, "m", _WirefanClient_sendOp).call(client, "unsubscribe", channel, { type: "unsubscribe", channel });
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
    publish(channel, data) {
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") !== "connected" || !__classPrivateFieldGet(this, _WirefanClient_ws, "f") || __classPrivateFieldGet(this, _WirefanClient_ws, "f").readyState !== WS_OPEN) {
            throw new ConnectionClosedError(`cannot publish while ${__classPrivateFieldGet(this, _WirefanClient_state, "f")}`);
        }
        __classPrivateFieldGet(this, _WirefanClient_ws, "f").send(JSON.stringify({ type: "publish", channel, data }));
    }
}
_WirefanClient_url = new WeakMap(), _WirefanClient_authorize = new WeakMap(), _WirefanClient_WS = new WeakMap(), _WirefanClient_reconnect = new WeakMap(), _WirefanClient_ackTimeoutMs = new WeakMap(), _WirefanClient_random = new WeakMap(), _WirefanClient_ws = new WeakMap(), _WirefanClient_state = new WeakMap(), _WirefanClient_socketId = new WeakMap(), _WirefanClient_closed = new WeakMap(), _WirefanClient_attempt = new WeakMap(), _WirefanClient_reconnectTimer = new WeakMap(), _WirefanClient_channels = new WeakMap(), _WirefanClient_pending = new WeakMap(), _WirefanClient_listeners = new WeakMap(), _WirefanClient_connectWaiters = new WeakMap(), _WirefanClient_instances = new WeakSet(), _WirefanClient_emit = function _WirefanClient_emit(event, payload) {
    const set = __classPrivateFieldGet(this, _WirefanClient_listeners, "f").get(event);
    if (!set)
        return;
    for (const h of [...set]) {
        try {
            h(payload);
        }
        catch {
            // A throwing listener must not break dispatch to the others.
        }
    }
}, _WirefanClient_setState = function _WirefanClient_setState(next) {
    if (next === __classPrivateFieldGet(this, _WirefanClient_state, "f"))
        return;
    const previous = __classPrivateFieldGet(this, _WirefanClient_state, "f");
    __classPrivateFieldSet(this, _WirefanClient_state, next, "f");
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "state", { state: next, previous });
}, _WirefanClient_dial = function _WirefanClient_dial() {
    let ws;
    try {
        ws = new (__classPrivateFieldGet(this, _WirefanClient_WS, "f"))(__classPrivateFieldGet(this, _WirefanClient_url, "f"));
    }
    catch (e) {
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onDrop).call(this, undefined, e instanceof Error ? e.message : String(e));
        return;
    }
    __classPrivateFieldSet(this, _WirefanClient_ws, ws, "f");
    ws.onmessage = (ev) => __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onMessage).call(this, ev.data);
    ws.onclose = (ev) => {
        if (__classPrivateFieldGet(this, _WirefanClient_ws, "f") !== ws)
            return;
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onDrop).call(this, ev.code, ev.reason);
    };
    ws.onerror = () => {
        // Some implementations fire error without close on dial failure.
        // The close handler is the single drop path; force it if needed.
        if (__classPrivateFieldGet(this, _WirefanClient_ws, "f") !== ws)
            return;
        if (ws.readyState !== WS_OPEN) {
            try {
                ws.close();
            }
            catch {
                // Already closed.
            }
        }
    };
    // The connection is not usable until the `connected` frame (§3.3);
    // onopen is intentionally not treated as "ready".
    ws.onopen = null;
}, _WirefanClient_onMessage = function _WirefanClient_onMessage(raw) {
    if (typeof raw !== "string")
        return; // protocol is text frames only (§2)
    let frame;
    try {
        frame = JSON.parse(raw);
    }
    catch {
        return; // unparseable server frame; ignore
    }
    switch (frame.type) {
        case "connected":
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onConnected).call(this, frame);
            return;
        case "subscribed":
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_settleOp).call(this, "subscribe", frame.channel);
            return;
        case "unsubscribed":
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_settleOp).call(this, "unsubscribe", frame.channel);
            return;
        case "event":
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onEvent).call(this, frame);
            return;
        case "error":
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_onErrorFrame).call(this, frame);
            return;
        default:
            // Unknown frame types must be ignored (§14 forward-compatibility).
            return;
    }
}, _WirefanClient_onConnected = function _WirefanClient_onConnected(frame) {
    __classPrivateFieldSet(this, _WirefanClient_socketId, frame.socket_id, "f");
    const reconnected = __classPrivateFieldGet(this, _WirefanClient_attempt, "f") > 0;
    __classPrivateFieldSet(this, _WirefanClient_attempt, 0, "f");
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_setState).call(this, "connected");
    if (frame.version !== "v1") {
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "error", new Error(`server speaks protocol "${frame.version}", client expects "v1"`));
    }
    const waiters = __classPrivateFieldGet(this, _WirefanClient_connectWaiters, "f");
    __classPrivateFieldSet(this, _WirefanClient_connectWaiters, [], "f");
    for (const w of waiters)
        w.resolve();
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "connected", { socketId: frame.socket_id, reconnected });
    if (reconnected)
        void __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_resubscribeAll).call(this);
}, _WirefanClient_onEvent = function _WirefanClient_onEvent(frame) {
    const rec = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(frame.channel);
    if (!rec)
        return;
    const ev = {
        channel: frame.channel,
        data: frame.data,
        id: frame.id,
    };
    for (const h of [...rec.handlers]) {
        try {
            h(ev);
        }
        catch {
            // A throwing handler must not break dispatch to the others.
        }
    }
}, _WirefanClient_onErrorFrame = function _WirefanClient_onErrorFrame(frame) {
    const err = new WirefanError(frame.code, frame.message);
    // Error frames carry no channel (§5.9). The server handles inbound frames
    // sequentially, so an error answering a control op arrives before any
    // later op's ack: attribute by code class to the oldest matching pending op.
    if (SUBSCRIBE_ERROR_CODES.has(frame.code)) {
        const idx = __classPrivateFieldGet(this, _WirefanClient_pending, "f").findIndex((op) => op.kind === "subscribe");
        if (idx !== -1) {
            const op = __classPrivateFieldGet(this, _WirefanClient_pending, "f").splice(idx, 1)[0];
            clearTimeout(op.timer);
            // The subscribe failed: forget the channel unless it was already
            // confirmed on this connection (idempotent re-subscribes).
            const rec = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(op.channel);
            if (rec && !rec.confirmed)
                __classPrivateFieldGet(this, _WirefanClient_channels, "f").delete(op.channel);
            op.reject(err);
            return;
        }
    }
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "error", err);
}, _WirefanClient_settleOp = function _WirefanClient_settleOp(kind, channel) {
    const idx = __classPrivateFieldGet(this, _WirefanClient_pending, "f").findIndex((op) => op.kind === kind && op.channel === channel);
    if (idx !== -1) {
        const op = __classPrivateFieldGet(this, _WirefanClient_pending, "f").splice(idx, 1)[0];
        clearTimeout(op.timer);
        op.resolve();
    }
    if (kind === "subscribe") {
        const rec = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(channel);
        if (rec)
            rec.confirmed = true;
    }
}, _WirefanClient_failPending = function _WirefanClient_failPending(err) {
    const pending = __classPrivateFieldGet(this, _WirefanClient_pending, "f");
    __classPrivateFieldSet(this, _WirefanClient_pending, [], "f");
    for (const op of pending) {
        clearTimeout(op.timer);
        op.reject(err);
    }
}, _WirefanClient_rejectConnectWaiters = function _WirefanClient_rejectConnectWaiters(err) {
    const waiters = __classPrivateFieldGet(this, _WirefanClient_connectWaiters, "f");
    __classPrivateFieldSet(this, _WirefanClient_connectWaiters, [], "f");
    for (const w of waiters)
        w.reject(err);
}, _WirefanClient_onDrop = function _WirefanClient_onDrop(code, reason) {
    if (__classPrivateFieldGet(this, _WirefanClient_closed, "f"))
        return;
    const ws = __classPrivateFieldGet(this, _WirefanClient_ws, "f");
    __classPrivateFieldSet(this, _WirefanClient_ws, null, "f");
    if (ws)
        ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    __classPrivateFieldSet(this, _WirefanClient_socketId, null, "f");
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_failPending).call(this, new ConnectionClosedError("connection dropped", code, reason));
    for (const rec of __classPrivateFieldGet(this, _WirefanClient_channels, "f").values())
        rec.confirmed = false;
    const canRetry = __classPrivateFieldGet(this, _WirefanClient_reconnect, "f") !== false && __classPrivateFieldGet(this, _WirefanClient_attempt, "f") + 1 <= __classPrivateFieldGet(this, _WirefanClient_reconnect, "f").maxAttempts;
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "disconnected", { willReconnect: canRetry, ...(code !== undefined ? { code } : {}), ...(reason ? { reason } : {}) });
    if (!canRetry) {
        __classPrivateFieldSet(this, _WirefanClient_closed, true, "f");
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_rejectConnectWaiters).call(this, new ConnectionClosedError("reconnect attempts exhausted", code, reason));
        for (const rec of __classPrivateFieldGet(this, _WirefanClient_channels, "f").values())
            rec.handlers.clear();
        __classPrivateFieldGet(this, _WirefanClient_channels, "f").clear();
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_setState).call(this, "closed");
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "closed", { reason: "exhausted" });
        return;
    }
    const r = __classPrivateFieldGet(this, _WirefanClient_reconnect, "f");
    __classPrivateFieldSet(this, _WirefanClient_attempt, __classPrivateFieldGet(this, _WirefanClient_attempt, "f") + 1, "f");
    const base = Math.min(r.maxDelayMs, r.initialDelayMs * Math.pow(r.multiplier, __classPrivateFieldGet(this, _WirefanClient_attempt, "f") - 1));
    const jittered = Math.round(base * (1 + r.jitter * (2 * __classPrivateFieldGet(this, _WirefanClient_random, "f").call(this) - 1)));
    const delayMs = Math.max(0, jittered);
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_setState).call(this, "reconnecting");
    __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "reconnecting", { attempt: __classPrivateFieldGet(this, _WirefanClient_attempt, "f"), delayMs });
    __classPrivateFieldSet(this, _WirefanClient_reconnectTimer, setTimeout(() => {
        __classPrivateFieldSet(this, _WirefanClient_reconnectTimer, null, "f");
        if (__classPrivateFieldGet(this, _WirefanClient_closed, "f"))
            return;
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_dial).call(this);
    }, delayMs), "f");
}, _WirefanClient_resubscribeAll = 
/**
 * After a reconnect, restore every channel the caller had. Tokens for
 * private-/presence- channels are re-fetched: the old ones were bound to
 * the previous socket_id and are single-use besides.
 */
async function _WirefanClient_resubscribeAll() {
    const channels = [...__classPrivateFieldGet(this, _WirefanClient_channels, "f").keys()];
    const restored = [];
    for (const channel of channels) {
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") !== "connected")
            return; // dropped again mid-restore
        const rec = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(channel);
        if (!rec)
            continue; // unsubscribed meanwhile
        try {
            await __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_ensureSubscribed).call(this, channel, rec);
            restored.push(channel);
        }
        catch (e) {
            // Surface the failure and drop the dead subscription rather than
            // silently pretending it is alive.
            __classPrivateFieldGet(this, _WirefanClient_channels, "f").delete(channel);
            __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "error", e instanceof Error
                ? e
                : new Error(`resubscribe ${channel} failed: ${String(e)}`));
        }
    }
    if (__classPrivateFieldGet(this, _WirefanClient_state, "f") === "connected" && restored.length > 0) {
        __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_emit).call(this, "resubscribed", { channels: restored });
    }
}, _WirefanClient_ensureSubscribed = function _WirefanClient_ensureSubscribed(channel, rec) {
    if (rec.confirmed)
        return Promise.resolve();
    if (!rec.inflight) {
        rec.inflight = __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_sendSubscribe).call(this, channel).finally(() => {
            const cur = __classPrivateFieldGet(this, _WirefanClient_channels, "f").get(channel);
            if (cur)
                cur.inflight = null;
        });
    }
    return rec.inflight;
}, _WirefanClient_sendSubscribe = async function _WirefanClient_sendSubscribe(channel) {
    const frame = {
        type: "subscribe",
        channel,
    };
    if (needsToken(channel)) {
        const socketId = __classPrivateFieldGet(this, _WirefanClient_socketId, "f");
        if (!socketId)
            throw new ConnectionClosedError("not connected");
        // Re-fetched every time: tokens are single-use and socket-bound.
        frame.token = await __classPrivateFieldGet(this, _WirefanClient_authorize, "f")({ socketId, channel });
        if (__classPrivateFieldGet(this, _WirefanClient_state, "f") !== "connected") {
            throw new ConnectionClosedError("connection dropped while authorizing");
        }
    }
    await __classPrivateFieldGet(this, _WirefanClient_instances, "m", _WirefanClient_sendOp).call(this, "subscribe", channel, frame);
}, _WirefanClient_sendOp = function _WirefanClient_sendOp(kind, channel, frame) {
    return new Promise((resolve, reject) => {
        const ws = __classPrivateFieldGet(this, _WirefanClient_ws, "f");
        if (!ws || ws.readyState !== WS_OPEN) {
            reject(new ConnectionClosedError("not connected"));
            return;
        }
        const op = {
            kind,
            channel,
            resolve,
            reject,
            timer: setTimeout(() => {
                const idx = __classPrivateFieldGet(this, _WirefanClient_pending, "f").indexOf(op);
                if (idx !== -1)
                    __classPrivateFieldGet(this, _WirefanClient_pending, "f").splice(idx, 1);
                reject(new AckTimeoutError(kind, channel, __classPrivateFieldGet(this, _WirefanClient_ackTimeoutMs, "f")));
            }, __classPrivateFieldGet(this, _WirefanClient_ackTimeoutMs, "f")),
        };
        __classPrivateFieldGet(this, _WirefanClient_pending, "f").push(op);
        try {
            ws.send(JSON.stringify(frame));
        }
        catch (e) {
            const idx = __classPrivateFieldGet(this, _WirefanClient_pending, "f").indexOf(op);
            if (idx !== -1)
                __classPrivateFieldGet(this, _WirefanClient_pending, "f").splice(idx, 1);
            clearTimeout(op.timer);
            reject(e instanceof Error ? e : new ConnectionClosedError("send failed"));
        }
    });
};
