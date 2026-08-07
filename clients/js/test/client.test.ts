import { describe, expect, it } from "vitest";
import {
  AckTimeoutError,
  ConfigurationError,
  ConnectionClosedError,
  WirefanClient,
  WirefanError,
  type ChannelEvent,
} from "../src/index.js";
import { FakeWSHarness, autoAccept, until } from "./fake-ws.js";

function makeClient(
  harness: FakeWSHarness,
  extra: Partial<ConstructorParameters<typeof WirefanClient>[0]> = {},
): WirefanClient {
  return new WirefanClient({
    url: "ws://localhost:8080/v1/connect",
    key: "k1",
    webSocket: harness.ctor,
    reconnect: false,
    ...extra,
  });
}

describe("construction", () => {
  it("requires url and key", () => {
    const h = new FakeWSHarness();
    expect(() => new WirefanClient({ url: "", key: "k", webSocket: h.ctor })).toThrow(
      ConfigurationError,
    );
    expect(
      () => new WirefanClient({ url: "ws://x", key: "", webSocket: h.ctor }),
    ).toThrow(ConfigurationError);
  });

  it("appends the key and default path to the URL", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "SID1");
    const c = makeClient(h, { url: "ws://localhost:8080" });
    await c.connect();
    expect(h.current.url).toBe("ws://localhost:8080/v1/connect?key=k1");
    c.close();
  });

  it("rewrites http(s) to ws(s)", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "SID1");
    const c = makeClient(h, { url: "https://example.com/v1/connect" });
    await c.connect();
    expect(h.current.url).toBe("wss://example.com/v1/connect?key=k1");
    c.close();
  });
});

describe("connect lifecycle", () => {
  it("resolves connect() on the connected frame, not on socket open", async () => {
    const h = new FakeWSHarness();
    const c = makeClient(h);
    let resolved = false;
    const p = c.connect().then(() => {
      resolved = true;
    });
    await until(() => h.sockets.length === 1, "dial");
    h.current.serverOpen();
    await new Promise((r) => setTimeout(r, 5));
    expect(resolved).toBe(false); // open alone is not ready
    h.current.serverSend({ type: "connected", socket_id: "SIDX", version: "v1" });
    await p;
    expect(c.state).toBe("connected");
    expect(c.socketId).toBe("SIDX");
    c.close();
  });

  it("emits a version-mismatch error but stays connected", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => {
      ws.serverOpen();
      ws.serverSend({ type: "connected", socket_id: "S", version: "v9" });
    };
    const c = makeClient(h);
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    await c.connect();
    expect(c.state).toBe("connected");
    expect(errors.some((e) => /protocol "v9"/.test(e.message))).toBe(true);
    c.close();
  });

  it("close() is terminal: connect() afterwards rejects", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();
    c.close();
    expect(c.state).toBe("closed");
    await expect(c.connect()).rejects.toBeInstanceOf(ConnectionClosedError);
  });
});

describe("subscribe / events / unsubscribe", () => {
  it("subscribes, receives events, and unsubscribes", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();

    const got: ChannelEvent[] = [];
    const sub = await c.subscribe("demo", (ev) => got.push(ev));
    expect(sub.active).toBe(true);
    expect(h.current.sentFrames()).toContainEqual({
      type: "subscribe",
      channel: "demo",
    });

    h.current.serverSend({
      type: "event",
      channel: "demo",
      data: { n: 1 },
      id: "01EVT",
    });
    expect(got).toEqual([{ channel: "demo", data: { n: 1 }, id: "01EVT" }]);

    await sub.unsubscribe();
    expect(sub.active).toBe(false);
    h.current.serverSend({
      type: "event",
      channel: "demo",
      data: { n: 2 },
      id: "01EVT2",
    });
    expect(got).toHaveLength(1); // handler detached
    c.close();
  });

  it("events for unsubscribed channels are ignored", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();
    // No throw, no handler call:
    h.current.serverSend({ type: "event", channel: "ghost", data: 1, id: "x" });
    c.close();
  });

  it("dedupes: second subscribe to the same channel sends no second frame", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();
    const a: unknown[] = [];
    const b: unknown[] = [];
    const s1 = await c.subscribe("demo", (ev) => a.push(ev.data));
    const s2 = await c.subscribe("demo", (ev) => b.push(ev.data));
    const subs = h.current
      .sentFrames()
      .filter((f) => f.type === "subscribe" && f.channel === "demo");
    expect(subs).toHaveLength(1);

    h.current.serverSend({ type: "event", channel: "demo", data: "x", id: "1" });
    expect(a).toEqual(["x"]);
    expect(b).toEqual(["x"]);

    // First handle detaching must not tear down the shared subscription.
    await s1.unsubscribe();
    expect(
      h.current.sentFrames().filter((f) => f.type === "unsubscribe"),
    ).toHaveLength(0);
    h.current.serverSend({ type: "event", channel: "demo", data: "y", id: "2" });
    expect(a).toEqual(["x"]);
    expect(b).toEqual(["x", "y"]);

    // Last handle detaching sends the unsubscribe frame.
    await s2.unsubscribe();
    expect(
      h.current.sentFrames().filter((f) => f.type === "unsubscribe"),
    ).toHaveLength(1);
    c.close();
  });

  it("rejects the subscribe promise on a server error frame, typed with the code", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h);
    await c.connect();
    const p = c.subscribe("_forbidden");
    await until(
      () => h.current.sentFrames().some((f) => f.type === "subscribe"),
      "subscribe frame",
    );
    h.current.serverSend({
      type: "error",
      code: "RESERVED_CHANNEL",
      message: "reserved",
    });
    const err = await p.then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(WirefanError);
    expect((err as WirefanError).code).toBe("RESERVED_CHANNEL");
    c.close();
  });

  it("emits unattributable error frames on the error event", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    await c.connect();
    // NOT_SUBSCRIBED is a publish-class error; no pending subscribe exists.
    h.current.serverSend({
      type: "error",
      code: "NOT_SUBSCRIBED",
      message: "publish to channel you are not subscribed to",
    });
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(WirefanError);
    expect((errors[0] as WirefanError).code).toBe("NOT_SUBSCRIBED");
    c.close();
  });

  it("times out an unacked subscribe", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 20 });
    await c.connect();
    await expect(c.subscribe("demo")).rejects.toBeInstanceOf(AckTimeoutError);
    c.close();
  });

  it("subscribe while not connected throws", async () => {
    const h = new FakeWSHarness();
    const c = makeClient(h);
    await expect(c.subscribe("demo")).rejects.toBeInstanceOf(
      ConnectionClosedError,
    );
    c.close();
  });
});

describe("private channels", () => {
  it("fetches a token via authorize and sends it in the subscribe frame", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "SID9");
    const calls: { socketId: string; channel: string }[] = [];
    const c = makeClient(h, {
      authorize: async (ctx) => {
        calls.push(ctx);
        return "tok-123";
      },
    });
    await c.connect();
    await c.subscribe("private-room");
    expect(calls).toEqual([{ socketId: "SID9", channel: "private-room" }]);
    expect(h.current.sentFrames()).toContainEqual({
      type: "subscribe",
      channel: "private-room",
      token: "tok-123",
    });
    c.close();
  });

  it("presence- channels also require authorize; missing authorize throws early", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h); // no authorize
    await c.connect();
    await expect(c.subscribe("presence-lobby")).rejects.toBeInstanceOf(
      ConfigurationError,
    );
    // Nothing was sent for it.
    expect(
      h.current.sentFrames().filter((f) => f.channel === "presence-lobby"),
    ).toHaveLength(0);
    c.close();
  });
});

describe("publish", () => {
  it("sends the publish frame verbatim", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();
    await c.subscribe("demo");
    c.publish("demo", { msg: "hello" });
    expect(h.current.lastFrame()).toEqual({
      type: "publish",
      channel: "demo",
      data: { msg: "hello" },
    });
    c.close();
  });

  it("throws when not connected", () => {
    const h = new FakeWSHarness();
    const c = makeClient(h);
    expect(() => c.publish("demo", 1)).toThrow(ConnectionClosedError);
    c.close();
  });
});
