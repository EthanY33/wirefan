import { describe, expect, it } from "vitest";
import {
  ConnectionClosedError,
  WirefanClient,
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
