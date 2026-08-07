// A deterministic in-memory WebSocket double. Tests script the "server" side
// by pushing frames and closes; the harness records every constructed socket
// so reconnect behaviour is observable.

import type { WebSocketLike } from "../src/index.js";

export class FakeWebSocket implements WebSocketLike {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  readonly url: string;
  readyState = FakeWebSocket.CONNECTING;
  sent: string[] = [];
  closedWith: { code?: number; reason?: string } | null = null;

  onopen: ((ev?: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code?: number; reason?: string }) => void) | null = null;
  onerror: ((ev?: unknown) => void) | null = null;

  constructor(url: string) {
    this.url = url;
  }

  send(data: string): void {
    if (this.readyState !== FakeWebSocket.OPEN) {
      throw new Error("send on non-open socket");
    }
    this.sent.push(data);
  }

  close(code?: number, reason?: string): void {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.closedWith = { code: code ?? 1000, reason: reason ?? "" };
    this.onclose?.({ code: code ?? 1000, reason: reason ?? "" });
  }

  // ---- server-side controls used by tests ----

  /** Complete the handshake: fires onopen. */
  serverOpen(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }

  /** Push a server frame to the client. */
  serverSend(frame: object | string): void {
    const data = typeof frame === "string" ? frame : JSON.stringify(frame);
    this.onmessage?.({ data });
  }

  /** Server-initiated close. */
  serverClose(code = 1001, reason = ""): void {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason });
  }

  /** Parse the sent frames as JSON. */
  sentFrames(): Record<string, unknown>[] {
    return this.sent.map((s) => JSON.parse(s) as Record<string, unknown>);
  }

  lastFrame(): Record<string, unknown> | undefined {
    return this.sentFrames().at(-1);
  }
}

/**
 * A factory that records every FakeWebSocket the client constructs, and can
 * script per-connection behaviour (e.g. auto-accepting the handshake).
 */
export class FakeWSHarness {
  sockets: FakeWebSocket[] = [];
  /** Called synchronously for each new socket; use to script the server. */
  onDial: ((ws: FakeWebSocket, index: number) => void) | null = null;

  readonly ctor: new (url: string) => FakeWebSocket;

  constructor() {
    const harness = this;
    this.ctor = class extends FakeWebSocket {
      constructor(url: string) {
        super(url);
        const idx = harness.sockets.length;
        harness.sockets.push(this);
        // Defer so the client can attach handlers after construction.
        queueMicrotask(() => harness.onDial?.(this, idx));
      }
    };
  }

  get current(): FakeWebSocket {
    const s = this.sockets.at(-1);
    if (!s) throw new Error("no socket dialed yet");
    return s;
  }
}

/**
 * Standard "well-behaved server" script: open the socket, send the connected
 * frame, then ack subscribes/unsubscribes for public channels.
 */
export function autoAccept(
  ws: FakeWebSocket,
  socketId: string,
  opts: { ackSubscribes?: boolean } = {},
): void {
  const ackSubscribes = opts.ackSubscribes ?? true;
  ws.serverOpen();
  if (ackSubscribes) {
    // Install the ack responder BEFORE the connected frame: the client starts
    // resubscribing synchronously while that frame is being dispatched.
    const origSend = ws.send.bind(ws);
    ws.send = (data: string) => {
      origSend(data);
      const frame = JSON.parse(data) as { type?: string; channel?: string };
      if (frame.type === "subscribe") {
        queueMicrotask(() =>
          ws.serverSend({ type: "subscribed", channel: frame.channel }),
        );
      } else if (frame.type === "unsubscribe") {
        queueMicrotask(() =>
          ws.serverSend({ type: "unsubscribed", channel: frame.channel }),
        );
      }
    };
  }
  ws.serverSend({ type: "connected", socket_id: socketId, version: "v1" });
}

/** Await until a predicate holds (bounded busy-wait over microtasks). */
export async function until(
  pred: () => boolean,
  what = "condition",
  ms = 2000,
): Promise<void> {
  const start = Date.now();
  while (!pred()) {
    if (Date.now() - start > ms) throw new Error(`timeout waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 1));
  }
}
