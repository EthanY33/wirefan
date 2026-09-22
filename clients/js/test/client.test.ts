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

describe("error routing", () => {
  // Settle-state probe that never blocks: "pending" until the promise settles.
  function track(p: Promise<unknown>): { state: string; value: unknown } {
    const t = { state: "pending", value: undefined as unknown };
    p.then(
      (v) => {
        t.state = "resolved";
        t.value = v;
      },
      (e: unknown) => {
        t.state = "rejected";
        t.value = e;
      },
    );
    return t;
  }

  function sentOf(h: FakeWSHarness, type: string): Record<string, unknown>[] {
    return h.current.sentFrames().filter((f) => f.type === type);
  }

  it("routes an error naming op and channel to that pending subscribe, not the oldest", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    await c.connect();
    const a = track(c.subscribe("a"));
    const b = track(c.subscribe("b"));
    await until(() => sentOf(h, "subscribe").length === 2, "both subscribe frames");

    h.current.serverSend({
      type: "error",
      code: "RATE_LIMITED",
      message: "too many control ops",
      op: "subscribe",
      channel: "b",
    });
    await until(() => b.state !== "pending", "subscribe b settles");
    expect(b.state).toBe("rejected");
    expect(b.value).toBeInstanceOf(WirefanError);
    expect((b.value as WirefanError).code).toBe("RATE_LIMITED");
    expect((b.value as WirefanError).op).toBe("subscribe");
    expect((b.value as WirefanError).channel).toBe("b");
    expect(a.state).toBe("pending"); // the older subscribe is not blamed

    h.current.serverSend({ type: "subscribed", channel: "a" });
    await until(() => a.state !== "pending", "subscribe a settles");
    expect(a.state).toBe("resolved");
    c.close();
  });

  it("does not blame a pending subscribe for a publish error", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    await c.connect();
    const a = track(c.subscribe("a"));
    await until(() => sentOf(h, "subscribe").length === 1, "subscribe frame");
    c.publish("_x", 1);

    h.current.serverSend({
      type: "error",
      code: "RESERVED_CHANNEL",
      message: "channel name reserved for server use",
      op: "publish",
      channel: "_x",
    });
    expect(errors).toHaveLength(1);
    expect((errors[0] as WirefanError).code).toBe("RESERVED_CHANNEL");
    expect((errors[0] as WirefanError).op).toBe("publish");
    expect((errors[0] as WirefanError).channel).toBe("_x");
    await new Promise((r) => setTimeout(r, 5));
    expect(a.state).toBe("pending");

    h.current.serverSend({ type: "subscribed", channel: "a" });
    await until(() => a.state !== "pending", "subscribe a settles");
    expect(a.state).toBe("resolved");
    c.close();
  });

  it("settles a pending unsubscribe on its own error instead of timing out", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) =>
      autoAccept(ws, "S", {
        respond: (f) => (f.type === "unsubscribe" ? null : undefined),
      });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    await c.connect();
    const sub = await c.subscribe("demo");
    const u = track(sub.unsubscribe());
    await until(() => sentOf(h, "unsubscribe").length === 1, "unsubscribe frame");

    h.current.serverSend({
      type: "error",
      code: "RATE_LIMITED",
      message: "too many control ops",
      op: "unsubscribe",
      channel: "demo",
    });
    await until(() => u.state !== "pending", "unsubscribe settles", 500);
    expect(u.state).toBe("rejected");
    expect(u.value).toBeInstanceOf(WirefanError);
    expect((u.value as WirefanError).code).toBe("RATE_LIMITED");
    c.close();
  });

  it("RATE_LIMITED_CONN answers subscribe and unsubscribe when the server names the op", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) =>
      autoAccept(ws, "S", {
        respond: (f) =>
          (f.type === "subscribe" && f.channel === "a") || f.type === "unsubscribe"
            ? {
                type: "error",
                code: "RATE_LIMITED_CONN",
                message: "too many frames on this connection",
                op: f.type,
                channel: f.channel,
              }
            : undefined,
      });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    await c.connect();

    const err = await c.subscribe("a").then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(WirefanError);
    expect((err as WirefanError).code).toBe("RATE_LIMITED_CONN");

    const sub = await c.subscribe("b");
    const uerr = await sub.unsubscribe().then(
      () => null,
      (e: unknown) => e,
    );
    expect(uerr).toBeInstanceOf(WirefanError);
    expect((uerr as WirefanError).code).toBe("RATE_LIMITED_CONN");
    c.close();
  });

  it("emits an error naming an operation that is no longer pending instead of misrouting it", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    const errors: Error[] = [];
    c.on("error", (e) => errors.push(e));
    await c.connect();
    const a = track(c.subscribe("a"));
    await until(() => sentOf(h, "subscribe").length === 1, "subscribe frame");

    h.current.serverSend({
      type: "error",
      code: "BAD_CHANNEL",
      message: "bad channel",
      op: "subscribe",
      channel: "gone",
    });
    expect(errors).toHaveLength(1);
    expect((errors[0] as WirefanError).code).toBe("BAD_CHANNEL");
    await new Promise((r) => setTimeout(r, 5));
    expect(a.state).toBe("pending");
    c.close();
  });

  it("falls back to oldest-subscribe attribution when the server sends neither op nor channel", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 1000 });
    await c.connect();
    const a = track(c.subscribe("a"));
    const b = track(c.subscribe("b"));
    await until(() => sentOf(h, "subscribe").length === 2, "both subscribe frames");

    h.current.serverSend({ type: "error", code: "RATE_LIMITED", message: "too many control ops" });
    await until(() => a.state !== "pending", "subscribe a settles");
    expect(a.state).toBe("rejected");
    expect((a.value as WirefanError).code).toBe("RATE_LIMITED");
    expect(b.state).toBe("pending");
    c.close();
  });

  it("unsubscribes when a subscribed ack arrives for a channel with no local record", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S", { ackSubscribes: false });
    const c = makeClient(h, { ackTimeoutMs: 20 });
    await c.connect();

    // The ack is late: the client gave up and forgot the channel.
    await expect(c.subscribe("late")).rejects.toBeInstanceOf(AckTimeoutError);
    h.current.serverSend({ type: "subscribed", channel: "late" });
    expect(sentOf(h, "unsubscribe")).toEqual([{ type: "unsubscribe", channel: "late" }]);
    c.close();
  });

  it("does not unsubscribe on a duplicate ack for a channel it still holds", async () => {
    const h = new FakeWSHarness();
    h.onDial = (ws) => autoAccept(ws, "S");
    const c = makeClient(h);
    await c.connect();
    await c.subscribe("demo");
    h.current.serverSend({ type: "subscribed", channel: "demo" });
    expect(sentOf(h, "unsubscribe")).toHaveLength(0);
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
