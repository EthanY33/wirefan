// End-to-end test against the real wirefan binary.
//
// Skips (with a visible reason) when the binary is not built. Build it first:
//   go build -o wirefan.exe ./cmd/wirefan     (repo root; drop .exe elsewhere)
// or point WIREFAN_BIN at a binary.

import { type ChildProcess, spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { WirefanClient, WirefanError } from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..", "..");

function findBinary(): string | null {
  const fromEnv = process.env.WIREFAN_BIN;
  if (fromEnv && existsSync(fromEnv)) return fromEnv;
  for (const name of ["wirefan.exe", "wirefan"]) {
    const p = join(repoRoot, name);
    if (existsSync(p)) return p;
  }
  return null;
}

const binary = findBinary();
const ADMIN_TOKEN = "itest-admin-token";
// Derive ports from the pid to keep parallel runs from colliding.
const port = 18100 + (process.pid % 500) * 2;
const adminPort = port + 1;
const baseUrl = `http://127.0.0.1:${port}`;
const adminUrl = `http://127.0.0.1:${adminPort}`;

let proc: ChildProcess | null = null;
let stateDir: string | null = null;
let keyId = "";
let keySecret = "";
let serverLog = "";

async function waitForHealth(ms = 15_000): Promise<void> {
  const start = Date.now();
  for (;;) {
    try {
      const res = await fetch(`${baseUrl}/v1/health`);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    if (proc && proc.exitCode !== null) {
      throw new Error(`server exited early (code ${proc.exitCode}):\n${serverLog}`);
    }
    if (Date.now() - start > ms) {
      throw new Error(`server did not become healthy in ${ms}ms:\n${serverLog}`);
    }
    await new Promise((r) => setTimeout(r, 100));
  }
}

function startServer(): void {
  // sqlite store: minted keys must survive the restart in the reconnect test.
  proc = spawn(
    binary!,
    [
      `--listen=127.0.0.1:${port}`,
      `--admin-addr=127.0.0.1:${adminPort}`,
      "--allowed-origins=*",
      "--dev",
      "--store=sqlite",
      `--db-path=${join(stateDir!, "itest.db")}`,
    ],
    {
      env: {
        ...process.env,
        WIREFAN_ADMIN_TOKEN: ADMIN_TOKEN,
        WIREFAN_STATE_DIR: stateDir!,
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  proc.stdout?.on("data", (d: Buffer) => (serverLog += d.toString()));
  proc.stderr?.on("data", (d: Buffer) => (serverLog += d.toString()));
}

async function stopServer(): Promise<void> {
  if (proc && proc.exitCode === null) {
    const p = proc;
    p.kill();
    await new Promise<void>((r) => {
      p.once("exit", () => r());
      setTimeout(r, 3000);
    });
  }
  proc = null;
}

describe.skipIf(!binary)("integration: real wirefan server", () => {
  beforeAll(async () => {
    stateDir = mkdtempSync(join(tmpdir(), "wirefan-itest-"));
    startServer();
    await waitForHealth();

    const res = await fetch(`${adminUrl}/v1/keys`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${ADMIN_TOKEN}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ name: "itest" }),
    });
    if (!res.ok) throw new Error(`mint key failed: ${res.status} ${await res.text()}`);
    const body = (await res.json()) as { id: string; secret: string };
    keyId = body.id;
    keySecret = body.secret;
  }, 30_000);

  afterAll(async () => {
    await stopServer();
    if (stateDir) rmSync(stateDir, { recursive: true, force: true });
  });

  it("connects, fans a publish out from one client to another", async () => {
    const a = new WirefanClient({ url: baseUrl, key: keyId, reconnect: false });
    const b = new WirefanClient({ url: baseUrl, key: keyId, reconnect: false });
    try {
      await a.connect();
      await b.connect();
      expect(a.socketId).toMatch(/^[0-9A-Z]{26}$/); // ULID
      expect(a.socketId).not.toBe(b.socketId);

      const received = new Promise<unknown>((resolveEv) => {
        void a.subscribe("itest-demo", (ev) => resolveEv(ev.data));
      });
      // a must be confirmed before b publishes; subscribe both, then publish.
      await a.subscribe("itest-demo");
      await b.subscribe("itest-demo");
      b.publish("itest-demo", { hello: "from-b" });
      expect(await received).toEqual({ hello: "from-b" });
    } finally {
      a.close();
      b.close();
    }
  });

  it("subscribes to a private channel via /v1/auth/sign and re-authorizes per subscribe", async () => {
    const authCalls: string[] = [];
    const client = new WirefanClient({
      url: baseUrl,
      key: keyId,
      reconnect: false,
      authorize: async ({ socketId, channel }) => {
        authCalls.push(channel);
        const res = await fetch(`${baseUrl}/v1/auth/sign`, {
          method: "POST",
          headers: {
            Authorization: `Bearer ${keyId}:${keySecret}`,
            "Content-Type": "application/json",
          },
          body: JSON.stringify({ socket_id: socketId, channel }),
        });
        if (!res.ok) {
          throw new Error(`sign failed: ${res.status} ${await res.text()}`);
        }
        const { token } = (await res.json()) as { token: string };
        return token;
      },
    });
    try {
      await client.connect();
      const got = new Promise<unknown>((resolveEv) => {
        void client.subscribe("private-itest", (ev) => resolveEv(ev.data));
      });
      await client.subscribe("private-itest");
      client.publish("private-itest", "secret-hello");
      expect(await got).toBe("secret-hello");
      expect(authCalls).toEqual(["private-itest"]);
    } finally {
      client.close();
    }
  });

  it("surfaces server rejections as typed WirefanError", async () => {
    const client = new WirefanClient({ url: baseUrl, key: keyId, reconnect: false });
    try {
      await client.connect();
      const err = await client.subscribe("_not-allowed").then(
        () => null,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(WirefanError);
      expect((err as WirefanError).code).toBe("RESERVED_CHANNEL");
    } finally {
      client.close();
    }
  });

  it("reconnects with a fresh socket_id and resubscribes after a server restart", async () => {
    const client = new WirefanClient({
      url: baseUrl,
      key: keyId,
      reconnect: { initialDelayMs: 100, maxDelayMs: 500, jitter: 0 },
    });
    try {
      await client.connect();
      const firstSid = client.socketId;
      const events: unknown[] = [];
      await client.subscribe("itest-reconnect", (ev) => events.push(ev.data));

      const reconnected = new Promise<void>((resolveEv) => {
        client.on("connected", ({ reconnected: re }) => {
          if (re) resolveEv();
        });
      });
      const resubscribed = new Promise<string[]>((resolveEv) => {
        client.on("resubscribed", ({ channels }) => resolveEv(channels));
      });

      // A real network drop: restart the server. The key survives (sqlite).
      await stopServer();
      startServer();
      await waitForHealth();

      await reconnected;
      expect(client.socketId).not.toBe(firstSid);
      expect(await resubscribed).toEqual(["itest-reconnect"]);

      // The restored subscription still delivers.
      const other = new WirefanClient({ url: baseUrl, key: keyId, reconnect: false });
      await other.connect();
      await other.subscribe("itest-reconnect");
      other.publish("itest-reconnect", "post-reconnect");
      const start = Date.now();
      while (!events.includes("post-reconnect")) {
        if (Date.now() - start > 5000) throw new Error("event not delivered after reconnect");
        await new Promise((r) => setTimeout(r, 25));
      }
      other.close();
    } finally {
      client.close();
    }
  }, 20_000);
});

// Always-on canary so a run without the binary still reports why it skipped.
describe.runIf(!binary)("integration (skipped)", () => {
  it("skipped: wirefan binary not found (set WIREFAN_BIN or build at repo root)", () => {
    expect(binary).toBeNull();
  });
});
