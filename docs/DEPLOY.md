# wirefan deployment runbook

Linear, copy-pasteable runbook for getting wirefan from "I just rented a
server" to a public `wss://` URL with TLS. Target: a **fresh Ubuntu 24.04
LTS VPS** with SSH and a public IPv4, on **amd64 or arm64**. Any provider
that rents you that works: Hetzner, DigitalOcean, Oracle Cloud, Lightsail,
Vultr. Nothing below touches a provider API.

Placeholder used throughout: `wirefan.example.com`. Substitute your real
hostname.

## What cannot run this (read before renting anything)

**Vercel, Netlify, Cloudflare Pages, GitHub Pages, and other serverless or
static hosts cannot run wirefan.** The reasons are structural, not
configuration problems:

- wirefan is one long-lived process holding thousands of open WebSocket
  connections. Serverless platforms run short-lived request handlers and
  kill anything that lingers.
- API keys live in a SQLite file on local disk. Serverless filesystems are
  ephemeral or read-only, so the keys would vanish on every cold start.
- Fanout state (channels, subscriptions) is in-process memory shared by all
  connections. That requires every client to hit the same single process,
  the opposite of what serverless scaling does.

You need a plain Linux box where a systemd service can run indefinitely.
The cheapest tier of any VPS provider (1 vCPU, 1 GB RAM) is enough to
start; see `docs/BENCHMARKS.md` for measured behavior under load.

Three facts the whole runbook leans on:

- **`--allowed-origins` is required.** wirefan refuses to start without it,
  and refuses `*` outside `--dev`.
- **The admin token is a file, not a log line.** On first boot wirefan
  writes a token to `/var/lib/wirefan/admin.token` (mode 0600) and reuses
  it on every later boot. It is never printed or logged.
- **The admin listener is separate from the public one.** `/v1/keys`,
  `/metrics`, and `/debug/pprof/*` live on `--admin-addr`
  (`127.0.0.1:6060`), which Caddy never exposes. Key minting and metric
  scraping happen from the host itself.

---

## 1. What to buy

- **A VPS**: Ubuntu 24.04 LTS image, 1 vCPU / 1 GB RAM minimum, amd64 or
  arm64, public IPv4, SSH key auth. Examples that fit: Hetzner CX22,
  DigitalOcean Basic Droplet, Oracle Always Free A1, Lightsail 1 GB,
  Vultr Cloud Compute.
- **A domain** (or a subdomain on one you own). Caddy gets a free
  Let's Encrypt certificate for it automatically; no cert purchase.

Create an SSH keypair if you don't have one:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/wirefan_vps
```

Give the provider the public key at instance creation, then:

```bash
ssh -i ~/.ssh/wirefan_vps ubuntu@<public-ip>   # user may be root/debian/etc. depending on provider
```

---

## 2. DNS

At your DNS provider, add an **A record** pointing your hostname at the
VPS:

```
wirefan.example.com  A  <public-ip>
```

If your DNS provider is Cloudflare: set the record to **DNS only (gray
cloud)**. Orange-cloud proxying intercepts the TLS handshake Caddy needs
for its ACME challenge and complicates WebSockets.

Verify before proceeding (Caddy cannot get a certificate until this
resolves):

```bash
dig +short wirefan.example.com
# expect: <public-ip>
```

---

## 3. Firewall

Three inbound TCP ports: **22** (SSH), **80** (ACME challenge + redirect),
**443** (TLS, where WebSockets live). Everything else stays closed. The
admin listener (6060) and the plaintext app listener (8080) bind loopback
and must NOT be opened.

Two layers to check:

**Provider firewall / security group / security list.** Most providers
block inbound by default (Oracle and Lightsail do; Hetzner and Vultr
default open). In the provider console, allow inbound TCP 22, 80, 443 from
`0.0.0.0/0`.

**Host firewall (ufw), on the server:**

```bash
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
sudo ufw status
```

---

## 4. Get a release binary

CGO note: wirefan's SQLite driver requires cgo, so binaries are
platform-specific. Releases ship `linux/amd64` and `linux/arm64` binaries
built on Ubuntu 24.04 runners (they link glibc 2.39, matching an Ubuntu
24.04 target). Check your arch with `uname -m`: `x86_64` means amd64,
`aarch64` means arm64.

**Option A: download from a GitHub release** (on the server):

```bash
VER=v1.0.0            # pick the release you want
ARCH=amd64            # or arm64, per uname -m
curl -fL -o wirefan_${VER}_linux_${ARCH} \
    https://github.com/EthanY33/wirefan/releases/download/${VER}/wirefan_${VER}_linux_${ARCH}
curl -fL -o SHA256SUMS \
    https://github.com/EthanY33/wirefan/releases/download/${VER}/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
# expect: wirefan_v1.0.0_linux_amd64: OK
```

**Option B: build locally and copy up.** From the repo root on a machine
with Docker (produces `dist/wirefan_linux_amd64`, `dist/wirefan_linux_arm64`,
and `dist/SHA256SUMS`):

```bash
make release-local
scp -i ~/.ssh/wirefan_vps dist/wirefan_linux_${ARCH} dist/SHA256SUMS ubuntu@<public-ip>:
```

**Option C: build on the server** (needs ~1 GB RAM free; the Go toolchain
plus gcc):

```bash
sudo apt-get install -y golang-go gcc git
git clone https://github.com/EthanY33/wirefan.git && cd wirefan
CGO_ENABLED=1 go build -o wirefan_local ./cmd/wirefan
```

---

## 5. Provision

Copy the repo's `deploy/` directory to the server (skip if you cloned the
repo in option C):

```bash
scp -i ~/.ssh/wirefan_vps -r deploy ubuntu@<public-ip>:
```

Then on the server, one command:

```bash
cd deploy
sudo ./provision.sh --domain wirefan.example.com --binary ../wirefan_v1.0.0_linux_amd64
```

The script is idempotent (safe to re-run) and stops at the first error.
It:

1. creates the `wirefan` system user (no shell, no home)
2. creates `/var/lib/wirefan` (admin token + SQLite db), `wirefan:wirefan`,
   mode 0700
3. installs `/etc/wirefan/env` from `.env.example` (mode 0600, never
   overwritten on re-run) and sets `WIREFAN_TRUSTED_PROXIES=127.0.0.1`,
   because Caddy proxies from loopback. Without that, wirefan's per-IP
   connection cap would attribute every connection to Caddy's IP and
   silently become a global 200-connection ceiling.
4. installs Caddy from its official apt repo
5. writes `/etc/caddy/Caddyfile` and `/etc/systemd/system/wirefan.service`
   with your domain substituted (public listener `:8080` loopback-proxied
   by Caddy; admin listener stays `127.0.0.1:6060`)
6. installs the binary at `/usr/local/bin/wirefan`
7. `systemctl enable --now wirefan` and reloads Caddy

It generates and prints no secrets. Caddy fetches the Let's Encrypt
certificate within a few seconds of the first HTTPS request, provided DNS
(step 2) and the firewall (step 3) are done; check with
`sudo journalctl -u caddy -n 50 --no-pager` and look for
`obtained certificate`.

---

## 6. Verify and mint the first API key

From your laptop:

```bash
curl -i https://wirefan.example.com/v1/health
# expect: HTTP/2 200, body: ok
```

The admin token was written by wirefan on first boot. Read it on the
server (it is deliberately never printed by any tooling):

```bash
sudo cat /var/lib/wirefan/admin.token
```

Mint a key, on the server (the admin listener is loopback-only by design):

```bash
curl -s -X POST http://127.0.0.1:6060/v1/keys \
    -H "Authorization: Bearer $(sudo cat /var/lib/wirefan/admin.token)" \
    -H "Content-Type: application/json" \
    -d '{"name":"production-app"}'
# expect: {"id":"01K...","name":"production-app","secret":"<hex>"}
```

The `secret` is shown **once**; store it in your app's config. It is only
needed for `private-`/`presence-` channel auth via `POST /v1/auth/sign`;
plain channels need only the key `id`. Keys persist in SQLite at
`/var/lib/wirefan/wirefan.db` and survive restarts and upgrades.

End-to-end pub/sub check from your laptop:

1. Open `https://wirefan.example.com/?key=<key-id>` in two browser tabs.
2. In each tab: connect, then subscribe to the demo channel.
3. Publish in tab A; tab B receives it.

The wire protocol for real clients is in `docs/PROTOCOL.md`.

---

## 7. Upgrade

Get the new binary and its checksum onto the server (step 4), then:

```bash
sudo ./deploy/deploy.sh ./wirefan_v1.1.0_linux_amd64 ./SHA256SUMS
```

`deploy.sh` verifies the SHA-256 (refusing on mismatch), stops the
service, keeps the current binary at `/usr/local/bin/wirefan.prev`, swaps
in the new one, starts, and polls `http://127.0.0.1:8080/v1/health` for up
to 30 seconds. **If the health check fails it automatically rolls back**
to the previous binary, restarts, and exits non-zero.

State survives upgrades: the admin token and API keys live in
`/var/lib/wirefan`, which the swap never touches. One behavior to know:
subscribe tokens for `private-`/`presence-` channels are signed with a
per-process secret, so any restart invalidates already-issued tokens;
clients must fetch a fresh one from `POST /v1/auth/sign` and reconnect.

Manual rollback later, if a problem surfaces after a green health check:

```bash
sudo ./deploy/deploy.sh /usr/local/bin/wirefan.prev \
    "$(sha256sum /usr/local/bin/wirefan.prev | awk '{print $1}')"
```

---

## 8. Backup and restore

Two files matter, both in `/var/lib/wirefan`: `wirefan.db` (SQLite, the
API keys) and `admin.token`. Everything else is rebuilt from `deploy/`.

Take a consistent snapshot of the db with SQLite's online backup (safe
while wirefan is running):

```bash
sudo apt-get install -y sqlite3
sudo sqlite3 /var/lib/wirefan/wirefan.db ".backup /tmp/wirefan-snapshot.db"
```

Pull snapshots to a backup machine on a cron:

```bash
# on the backup machine
rsync -avz -e "ssh -i ~/.ssh/wirefan_vps" \
    ubuntu@<public-ip>:/tmp/wirefan-snapshot.db \
    ./backups/wirefan-$(date +%Y%m%d).db
```

Restore (also the full-host-loss story: provision a fresh box via steps
1-5, then restore):

```bash
sudo systemctl stop wirefan
sudo cp ./backups/wirefan-20260801.db /var/lib/wirefan/wirefan.db
sudo chown wirefan:wirefan /var/lib/wirefan/wirefan.db
sudo chmod 0600 /var/lib/wirefan/wirefan.db
sudo systemctl start wirefan
curl -fsS https://wirefan.example.com/v1/health
```

Back up `admin.token` once (or set `WIREFAN_ADMIN_TOKEN` in
`/etc/wirefan/env` and treat that file as the secret to manage). If you
lose it, delete `/var/lib/wirefan/admin.token` and restart: wirefan mints
a fresh token; existing API keys are unaffected.

---

## 9. Metrics

Prometheus metrics are on the **admin listener**, never the public one:

```bash
# on the server
curl -s http://127.0.0.1:6060/metrics | grep '^wirefan_' | head -20
```

Useful series to watch: connection counts, per-channel subscriber gauges,
publish/deliver counters (their ratio is your fanout amplification), and
dropped-message counters (nonzero means slow consumers are being dropped,
which is the documented at-most-once behavior, not a bug). Histograms are
documented in `docs/DESIGN.md`.

For external scraping, do not open 6060 to the internet. Either run
Prometheus/Grafana Agent on the VPS itself, or put the scraper and the VPS
on a WireGuard/Tailscale network and scrape `127.0.0.1:6060` over that.

Ad-hoc profiling uses the same listener:

```bash
curl -s http://127.0.0.1:6060/debug/pprof/heap > /tmp/heap.pprof
go tool pprof -top /tmp/heap.pprof
```

---

## 10. Day-to-day operations

```bash
sudo systemctl status wirefan            # is it up
sudo systemctl restart wirefan           # restart (keys + token survive)
sudo journalctl -u wirefan -f            # follow logs
sudo journalctl -u wirefan --since "10 min ago"
sudo journalctl -u caddy -n 50 --no-pager   # TLS/proxy issues live here
top -p "$(pgrep -x wirefan)"             # resource usage
```

The most common first-boot failure is a missing or invalid
`--allowed-origins`: wirefan exits immediately with a usage error rather
than starting insecurely. During graceful shutdown `/v1/health` flips to
`503` with body `draining` so load balancers can drain; steady state is
`200` `ok`.

---

## Appendix A: Docker instead of a binary

The systemd-plus-binary path above is the recommended one (smallest moving
parts, full unit hardening). A container path exists too:
`deploy/Dockerfile` builds a distroless image; `deploy/README.md` shows
how to run and smoke-test it, including the two container-specific
requirements (`--admin-addr=0.0.0.0:6060` plus a `127.0.0.1`-bound port
publish, and a volume over `/var/lib/wirefan` owned by uid 65532).

## Appendix B: no public IP? Cloudflare Tunnel

If you are running on a home machine behind NAT instead of a VPS,
Cloudflare Tunnel works: run `cloudflared` pointing
`wirefan.example.com` at `http://localhost:8080`, run wirefan with
`WIREFAN_TRUSTED_PROXIES=127.0.0.1` and
`--allowed-origins=https://wirefan.example.com`, and skip Caddy entirely
(Cloudflare terminates TLS at its edge). Tradeoffs: availability tracks
the home machine, and Cloudflare sees decrypted traffic.

---

## Cross-references

- `deploy/provision.sh`: the fresh-box script (step 5).
- `deploy/deploy.sh`: the upgrade/rollback script (step 7).
- `deploy/wirefan.service`: systemd unit with hardening flags.
- `deploy/Caddyfile`: reverse-proxy + auto-TLS config.
- `deploy/.env.example`: documents `WIREFAN_TRUSTED_PROXIES`,
  `WIREFAN_STATE_DIR`, `WIREFAN_IP_CAP`, `WIREFAN_ADMIN_TOKEN`.
- `.github/workflows/release.yml`: builds the release binaries on tag push.
- `docs/PROTOCOL.md`: wire protocol clients implement.
- `docs/DESIGN.md`: runtime architecture, fanout, backpressure.
- `docs/BENCHMARKS.md`: benchmark methodology and published numbers.
