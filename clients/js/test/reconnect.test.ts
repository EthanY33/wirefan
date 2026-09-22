import { describe, expect, it } from "vitest";
import {
  AckTimeoutError,
  ConnectionClosedError,
  WirefanClient,
  WirefanError,
  type WirefanState,
} from "../src/index.js";
import { FakeWSHarness, autoAccept, until } from "./fake-ws.js";

const FAST = {
  initialDelayMs: 1,
  maxDelayMs: 8,
  multiplier: 2,
  jitter: 0,
};

function makeClient(
  harness: FakeWSHarness,
  extra: Partial<ConstructorParameters<typeof WirefanClient>[0]> = {},
): WirefanClient {
  return new WirefanClient({
    url: "ws://localhost:8080/v1/connect",
    key: "k1",
    webSocket: harness.ctor,
    reconnect: FAST,
    random: () => 0.5, // deterministic jitter midpoint
    ...extra,
  });
}

describe("reconnect", () => {
  it("reconnects after an unexpected close and resubscribes every channel", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`);
    const c = makeClient(h);
    const states: WirefanState[] = [];
    c.on("state", (s) => states.push(s.state));
    const resub: string[][] = [];
    c.on("resubscribed", (r) => resub.push(r.channels));

    await c.connect();
    const got: unknown[] = [];
    await c.subscribe("demo", (ev) => got.push(ev.data));
    await c.subscribe("other");

    // Server drops us (e.g. 1001 idle deadline).
    h.sockets[0]!.serverClose(1001, "going away");
    await until(() => c.state === "connected" && h.sockets.length === 2, "reconnect");

    expect(c.socketId).toBe("SID1");
    await until(() => resub.length === 1, "resubscribed event");
    expect(resub[0]!.sort()).toEqual(["demo", "other"]);
    const subs = h.sockets[1]!
      .sentFrames()
      .filter((f) => f.type === "subscribe")
      .map((f) => f.channel);
    expect(subs.sort()).toEqual(["demo", "other"]);

    // Handler still attached after reconnect.
    h.sockets[1]!.serverSend({ type: "event", channel: "demo", data: 42, id: "e" });
    expect(got).toEqual([42]);

    expect(states).toContain("reconnecting");
    c.close();
  });

  it("re-fetches tokens for private channels on resubscribe", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`);
    const tokens: string[] = [];
    let n = 0;
    const c = makeClient(h, {
      authorize: ({ socketId, channel }) => {
        const t = `tok-${socketId}-${channel}-${n++}`;
        tokens.push(t);
        return t;
      },
    });
    await c.connect();
    await c.subscribe("private-room");
    h.sockets[0]!.serverClose(1006, "");
    await until(() => c.state === "connected" && h.sockets.length === 2, "reconnect");
    await until(
      () => h.sockets[1]!.sentFrames().some((f) => f.type === "subscribe"),
      "resubscribe frame",
    );
    // Two distinct tokens, second bound to the new socket id.
    expect(tokens).toHaveLength(2);
    expect(tokens[1]).toContain("SID1");
    const frame = h.sockets[1]!
      .sentFrames()
      .find((f) => f.type === "subscribe")!;
    expect(frame.token).toBe(tokens[1]);
    c.close();
  });

  it("abandons an authorize() left in flight by a drop and re-fetches a token for the new socket", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`);
    const calls: string[] = [];
    let releaseStale!: (token: string) => void;
    const c = makeClient(h, {
      authorize: ({ socketId }) => {
        calls.push(socketId);
        if (calls.length === 1) {
          // First authorize hangs until the test releases it, simulating a
          // /v1/auth/sign round trip that outlives the WebSocket.
          return new Promise<string>((resolve) => {
            releaseStale = resolve;
          });
        }
        return Promise.resolve(`tok-for-${socketId}`);
      },
    });
    await c.connect();
    const sub = c.subscribe("private-room", () => {});
    const subOutcome = sub.then(
      () => null,
      (e: unknown) => e,
    );
    await until(() => calls.length === 1, "first authorize");

    // Drop while authorize() is still pending, then reconnect.
    h.sockets[0]!.serverClose(1006, "blip");
    await until(() => c.state === "connected" && h.sockets.length === 2, "reconnect");

    // The resubscribe must start a FRESH attempt (second authorize call,
    // bound to the new socket), not adopt the stale in-flight one.
    await until(() => calls.length === 2, "authorize re-invoked after reconnect");
    expect(calls).toEqual(["SID0", "SID1"]);

    // Now the stale authorize resolves with a token minted for the dead
    // socket. It must never reach the wire.
    releaseStale("tok-for-SID0");
    await until(
      () => h.sockets[1]!.sentFrames().some((f) => f.type === "subscribe"),
      "resubscribe frame",
    );
    await new Promise((r) => setTimeout(r, 20)); // give the stale path time to (wrongly) send
    const subs = h.sockets[1]!.sentFrames().filter((f) => f.type === "subscribe");
    expect(subs).toEqual([
      { type: "subscribe", channel: "private-room", token: "tok-for-SID1" },
    ]);
    // The interrupted subscribe() call surfaces the drop instead of lying.
    expect(await subOutcome).toBeInstanceOf(ConnectionClosedError);
    c.close();
  });

  it("times out a handshake that never produces the connected frame and retries", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => {
      // First dial: the upgrade succeeds but the server goes silent (no
      // `connected` frame, no close). Second dial behaves.
      if (i === 0) ws.serverOpen();
      else autoAccept(ws, `SID${i}`);
    };
    const c = makeClient(h, { handshakeTimeoutMs: 20 });
    const events: string[] = [];
    c.on("disconnected", () => events.push("disconnected"));
    c.on("reconnecting", () => events.push("reconnecting"));
    await c.connect(); // resolves via the second dial, not by hanging forever
    expect(h.sockets.length).toBe(2);
    expect(c.socketId).toBe("SID1");
    expect(events).toContain("disconnected");
    expect(events).toContain("reconnecting");
    expect(h.sockets[0]!.closedWith?.reason).toBe("handshake-timeout");
    c.close();
  });

  it("handshake timeout with reconnect disabled closes the client instead of wedging it", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => ws.serverOpen(); // silent server
    const c = makeClient(h, { reconnect: false, handshakeTimeoutMs: 20 });
    const closed = new Promise<string>((resolve) => {
      c.on("closed", (ev) => resolve(ev.reason));
    });
    const err = await c.connect().then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ConnectionClosedError);
    expect(await closed).toBe("exhausted");
    expect(c.state).toBe("closed");
  });

  it("uses exponential backoff with a cap and deterministic jitter", async () => {
    const h = new FakeWSHarness();
    // Never accept: every dial is closed immediately.
    h.onDial = (ws) => ws.serverClose(1006, "refused");
    const c = makeClient(h, {
      reconnect: { ...FAST, maxAttempts: 5 },
    });
    const delays: number[] = [];
    c.on("reconnecting", (r) => delays.push(r.delayMs));
    const closed = new Promise<string>((resolve) => {
      c.on("closed", (ev) => resolve(ev.reason));
    });
    const err = await c.connect().then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ConnectionClosedError);
    expect(await closed).toBe("exhausted");
    // initial 1ms, x2 each attempt, capped at 8ms; jitter=0.
    expect(delays).toEqual([1, 2, 4, 8, 8]);
    expect(c.state).toBe("closed");
    expect(h.sockets.length).toBe(6); // initial dial + 5 retries
  });

  it("applies jitter from the injected random source", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => ws.serverClose(1006, "refused");
    const c = makeClient(h, {
      reconnect: { initialDelayMs: 100, maxDelayMs: 100, multiplier: 1, jitter: 0.5, maxAttempts: 1 },
      random: () => 1, // +full jitter -> 100 * 1.5
    });
    const delays: number[] = [];
    c.on("reconnecting", (r) => delays.push(r.delayMs));
    await c.connect().catch(() => {});
    expect(delays).toEqual([150]);
  });

  it("never reconnects after an explicit close", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`);
    const c = makeClient(h);
    await c.connect();
    c.close();
    // Give any (buggy) reconnect timer a chance to fire.
    await new Promise((r) => setTimeout(r, 30));
    expect(h.sockets.length).toBe(1);
    expect(c.state).toBe("closed");
  });

  it("rejects in-flight subscribes when the connection drops", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`, { ackSubscribes: false });
    const c = makeClient(h);
    await c.connect();
    const p = c.subscribe("demo");
    await until(
      () => h.current.sentFrames().some((f) => f.type === "subscribe"),
      "subscribe frame",
    );
    h.sockets[0]!.serverClose(1006, "dropped");
    await expect(p).rejects.toBeInstanceOf(ConnectionClosedError);
    c.close();
  });

  it("keeps connect() pending across failed attempts and resolves when a later attempt succeeds", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => {
      if (i < 2) ws.serverClose(1006, "refused");
      else autoAccept(ws, "SID-final");
    };
    const c = makeClient(h);
    await c.connect(); // resolves on the third dial
    expect(c.socketId).toBe("SID-final");
    expect(h.sockets.length).toBe(3);
    c.close();
  });

  it("reports reconnected=false on the first connection even when the first dial failed", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) => {
      if (i === 0) ws.serverClose(1006, "refused");
      else autoAccept(ws, `SID${i}`);
    };
    const c = makeClient(h);
    const seen: boolean[] = [];
    c.on("connected", (ev) => seen.push(ev.reconnected));
    await c.connect(); // resolves on the second dial
    expect(h.sockets.length).toBe(2);
    expect(seen).toEqual([false]);

    // A later drop and recovery is a genuine reconnect.
    h.sockets[1]!.serverClose(1006, "blip");
    await until(() => seen.length === 2, "second connected event");
    expect(seen).toEqual([false, true]);
    c.close();
  });

  it("emits disconnected with willReconnect=false when reconnect is disabled", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h, { reconnect: false });
    const events: boolean[] = [];
    c.on("disconnected", (d) => events.push(d.willReconnect));
    await c.connect();
    h.sockets[0]!.serverClose(1001, "bye");
    await until(() => c.state === "closed", "closed state");
    expect(events).toEqual([false]);
    expect(h.sockets.length).toBe(1);
  });
});

describe("resubscribe failures", () => {
  function subscribesOn(h: FakeWSHarness, i: number): Record<string, unknown>[] {
    return h.sockets[i]!.sentFrames().filter((f) => f.type === "subscribe");
  }

  it("keeps a channel whose resubscribe was cut by a second drop and restores it on the next connection", async () => {
    const h = new FakeWSHarness();
    // The second connection never acks, so its resubscribe is still in
    // flight when that connection drops too.
    h.onDial = (ws, i) => autoAccept(ws, `SID${i}`, { ackSubscribes: i !== 1 });
    const c = makeClient(h);
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    const resub: string[][] = [];
    c.on("resubscribed", (r) => resub.push(r.channels));
    await c.connect();
    const got: unknown[] = [];
    await c.subscribe("demo", (ev) => got.push(ev.data));

    h.sockets[0]!.serverClose(1006, "blip");
    await until(
      () => h.sockets.length === 2 && subscribesOn(h, 1).length === 1,
      "resubscribe frame on the second connection",
    );
    h.sockets[1]!.serverClose(1006, "blip again");
    await until(() => h.sockets.length === 3 && c.state === "connected", "third connection");
    await until(() => resub.length === 1, "resubscribed event");

    expect(resub[0]).toEqual(["demo"]);
    expect(subscribesOn(h, 2)).toEqual([{ type: "subscribe", channel: "demo" }]);
    h.sockets[2]!.serverSend({ type: "event", channel: "demo", data: "still here", id: "e" });
    expect(got).toEqual(["still here"]);
    // A drop is not a channel failure: the disconnected event covers it.
    expect(errors).toEqual([]);
    c.close();
  });

  it("retries a resubscribe whose ack timed out", async () => {
    const h = new FakeWSHarness();
    let seen = 0;
    h.onDial = (ws, i) =>
      autoAccept(ws, `SID${i}`, {
        // The first resubscribe on the second connection goes unanswered.
        respond: (f) => (i === 1 && f.type === "subscribe" && ++seen === 1 ? null : undefined),
      });
    const c = makeClient(h, { ackTimeoutMs: 30 });
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    const resub: string[][] = [];
    c.on("resubscribed", (r) => resub.push(r.channels));
    await c.connect();
    const got: unknown[] = [];
    await c.subscribe("demo", (ev) => got.push(ev.data));

    h.sockets[0]!.serverClose(1006, "blip");
    await until(() => resub.length === 1, "resubscribed after the retry");

    expect(resub[0]).toEqual(["demo"]);
    expect(subscribesOn(h, 1)).toHaveLength(2);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(AckTimeoutError);
    h.sockets[1]!.serverSend({ type: "event", channel: "demo", data: 1, id: "e" });
    expect(got).toEqual([1]);
    c.close();
  });

  it.each([
    // Older server: the error frame names no op, so it is attributed FIFO.
    ["RATE_LIMITED", false],
    // Newer server: op and channel route it exactly.
    ["RATE_LIMITED", true],
    ["RATE_LIMITED_CONN", true],
  ])(
    "retries a resubscribe refused with %s (op named: %s) after a backoff",
    async (code, named) => {
      const h = new FakeWSHarness();
      let seen = 0;
      h.onDial = (ws, i) =>
        autoAccept(ws, `SID${i}`, {
          // A reconnect herd: the first two resubscribes are rate limited.
          respond: (f) => {
            if (i !== 1 || f.type !== "subscribe" || ++seen > 2) return undefined;
            return {
              type: "error",
              code,
              message: "slow down",
              ...(named ? { op: "subscribe", channel: f.channel } : {}),
            };
          },
        });
      // The default 10 s ack timeout rules out a retry driven by the timer.
      const c = makeClient(h);
      const errors: Error[] = [];
      c.on("error", (e) => errors.push(e));
      const resub: string[][] = [];
      c.on("resubscribed", (r) => resub.push(r.channels));
      await c.connect();
      const got: unknown[] = [];
      await c.subscribe("demo", (ev) => got.push(ev.data));

      h.sockets[0]!.serverClose(1006, "blip");
      await until(() => resub.length === 1, "resubscribed after the retries");

      expect(resub[0]).toEqual(["demo"]);
      expect(subscribesOn(h, 1)).toHaveLength(3);
      expect(errors.map((e) => (e as WirefanError).code)).toEqual([code, code]);
      h.sockets[1]!.serverSend({ type: "event", channel: "demo", data: 1, id: "e" });
      expect(got).toEqual([1]);
      c.close();
    },
  );

  it.each([
    "AUTH_FAILED",
    "AUTH_REPLAYED",
    "RESERVED_CHANNEL",
    "BAD_CHANNEL",
    "LIMIT_CHANNELS",
    "LIMIT_SUBSCRIBERS",
    "SUBSCRIBE_FAILED",
  ])("drops a channel whose resubscribe is definitively refused with %s", async (code) => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) =>
      autoAccept(ws, `SID${i}`, {
        respond: (f) =>
          i === 1 && f.type === "subscribe"
            ? { type: "error", code, message: "no", op: "subscribe", channel: f.channel }
            : undefined,
      });
    const c = makeClient(h);
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    await c.connect();
    const sub = await c.subscribe("demo", () => {});

    h.sockets[0]!.serverClose(1006, "blip");
    await until(() => errors.length === 1, "resubscribe error");
    expect(errors[0]).toBeInstanceOf(WirefanError);
    expect((errors[0] as WirefanError).code).toBe(code);

    await new Promise((r) => setTimeout(r, 40)); // well past any FAST backoff
    expect(subscribesOn(h, 1)).toHaveLength(1);
    expect(sub.active).toBe(false);
    c.close();
  });

  it("stops retrying a rate-limited resubscribe once the caller unsubscribes", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws, i) =>
      autoAccept(ws, `SID${i}`, {
        respond: (f) =>
          i === 1 && f.type === "subscribe"
            ? { type: "error", code: "RATE_LIMITED", message: "slow down", op: "subscribe", channel: f.channel }
            : undefined,
      });
    const c = makeClient(h);
    await c.connect();
    const sub = await c.subscribe("demo", () => {});

    h.sockets[0]!.serverClose(1006, "blip");
    await until(() => h.sockets.length === 2 && subscribesOn(h, 1).length >= 2, "a retry");
    await sub.unsubscribe();
    const sent = subscribesOn(h, 1).length;
    await new Promise((r) => setTimeout(r, 40)); // several FAST backoff periods
    expect(subscribesOn(h, 1)).toHaveLength(sent);
    c.close();
  });
});
