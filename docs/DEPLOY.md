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
A single-vCPU VPS is enough to start: `docs/BENCHMARKS.md` records
measured behavior with the server capped at one CPU of quota
(`docker run --cpus=1`). Those runs did not
constrain memory, so this runbook makes no memory-sizing claim; the
smallest tier your provider sells is the natural starting point.

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

- **A VPS**: Ubuntu 24.04 LTS image, 1 vCPU or more (see the memory note
  above), amd64 or arm64, public IPv4, SSH key auth. Examples that fit:
  Hetzner CX22,
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

If your DNS provider is Cloudflare, you have a choice here:

- **DNS only (gray cloud)** keeps the direct-to-origin setup this runbook
  documents. Caddy does its ACME challenge and clients connect straight to
  the VPS. This is the simpler default; the rest of the runbook assumes it.
- **Proxied (orange cloud)** puts Cloudflare's network in front of the
  origin: DDoS absorption, WAF, and the origin IP hidden from the public
  DNS answer. This changes TLS, the firewall, and one wirefan setting that
  causes an outage if missed. Do NOT just flip the cloud orange; follow
  **Appendix C** instead.

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
admin listener (`127.0.0.1:6060`) and the plaintext app listener
(`127.0.0.1:8080`, reached only through Caddy) bind IPv4 loopback and
need no firewall rule.

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

(If you are going Cloudflare-proxied per Appendix C, these open-world 80
and 443 rules get replaced in C.3; set them up as above first so the
initial provisioning in step 5 works, then tighten.)

---

## 4. Get a release binary

CGO note: wirefan's SQLite driver requires cgo, so binaries are
platform-specific. Releases ship `linux/amd64` and `linux/arm64` binaries
built natively on Ubuntu 24.04 runners, so they link the runner's glibc
and match an Ubuntu 24.04 target; the release workflow's smoke-test step
prints `ldd --version` so the exact glibc is on record in every build
log.

Every option below leaves the binary in your home directory on the
server under the release's own name, `wirefan_<version>_linux_<arch>`
(for example `wirefan_v1.0.0_linux_amd64`), and the later steps refer to
it by that name. Set the two parts once in each shell you run the
snippets in, laptop or server:

```bash
VER=v1.0.0    # the release tag you are deploying
ARCH=amd64    # the SERVER's arch: amd64 if its `uname -m` prints x86_64, arm64 if aarch64
```

**Option A: download from a GitHub release** (on the server). Releases
exist from the first `v1.x` tag onward; until that tag is pushed these
URLs return 404 and Option B is the path:

```bash
cd ~
curl -fL -o wirefan_${VER}_linux_${ARCH} \
    https://github.com/EthanY33/wirefan/releases/download/${VER}/wirefan_${VER}_linux_${ARCH}
curl -fL -o SHA256SUMS \
    https://github.com/EthanY33/wirefan/releases/download/${VER}/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
# expect: wirefan_v1.0.0_linux_amd64: OK
```

**Option B: build locally and copy up.** From a checkout of the tag on a
machine with Docker. `make release-local` writes
`dist/wirefan_<version>_linux_amd64`, `dist/wirefan_<version>_linux_arm64`
and `dist/SHA256SUMS`, the same names the release workflow uses, where
`<version>` is `git describe --tags --always --dirty`: exactly the tag on
a clean tag checkout. (Built from any other commit, the name carries the
describe output, such as `v1.0.0-3-gabc1234`; set `VER` to that.)

```bash
git checkout ${VER}
make release-local
scp -i ~/.ssh/wirefan_vps dist/wirefan_${VER}_linux_${ARCH} dist/SHA256SUMS ubuntu@<public-ip>:
# then on the server, in ~: sha256sum -c --ignore-missing SHA256SUMS
```

**Option C: build on the server** (needs ~1 GB RAM free, gcc, and Go
1.26 or newer). Ubuntu 24.04's `golang-go` package is Go 1.22, too old
for this module, so install the official build from
<https://go.dev/dl/> (any 1.26.x; 1.26.8 below). Alternatively keep a
distro Go of 1.21 or later and put `GOTOOLCHAIN=auto` in front of the
`go build` line: Go then downloads the toolchain `go.mod` asks for.

```bash
sudo apt-get install -y gcc git
sudo rm -rf /usr/local/go    # the official install replaces any previous one
curl -fL https://go.dev/dl/go1.26.8.linux-${ARCH}.tar.gz | sudo tar -C /usr/local -xz
export PATH=/usr/local/go/bin:$PATH
go version                   # expect: go version go1.26.8 linux/<arch>
cd ~ && git clone https://github.com/EthanY33/wirefan.git && cd wirefan
git checkout ${VER}
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -X main.version=${VER}" \
    -o ~/wirefan_${VER}_linux_${ARCH} ./cmd/wirefan
cd ~
```

Whichever option you used, confirm the binary runs on this server and is
the version you meant. `--version` works without any other flag:

```bash
chmod +x ~/wirefan_${VER}_linux_${ARCH}
~/wirefan_${VER}_linux_${ARCH} --version
# expect: wirefan v1.0.0 (go1.26.x, linux/amd64)
```

---

## 5. Provision

Copy the repo's `deploy/` directory to the server (skip if you cloned the
repo in Option C; its copy is `~/wirefan/deploy`):

```bash
scp -i ~/.ssh/wirefan_vps -r deploy ubuntu@<public-ip>:
```

Then on the server, with `VER` and `ARCH` set as in step 4, one command:

```bash
cd ~/deploy    # Option C: cd ~/wirefan/deploy
sudo ./provision.sh --domain wirefan.example.com --binary ~/wirefan_${VER}_linux_${ARCH}
```

The script is idempotent (safe to re-run) and stops at the first error.
Re-runs rewrite the derived files (systemd unit, Caddyfile), saving a
`.bak` of any previous version they change, and restart wirefan so a
changed unit or binary takes effect; `/etc/wirefan/env` is never
overwritten. If the box hosts other Caddy sites, know that the Caddyfile
is wholly owned by this script: merge other sites back from the `.bak`.
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
   with your domain substituted (public listener `127.0.0.1:8080`, which
   Caddy proxies to; admin listener `127.0.0.1:6060`)
6. installs the binary at `/usr/local/bin/wirefan`
7. `systemctl enable wirefan`, restarts it if a binary is installed, and
   reloads Caddy either way so the new Caddyfile replaces Caddy's stock
   config

It generates and prints no secrets. Caddy requests the Let's Encrypt
certificate as soon as it loads the new Caddyfile (the reload at the end
of the script), typically done well under a minute later, provided DNS
(step 2) and the firewall (step 3) are done; check with
`sudo journalctl -u caddy -n 50 --no-pager` and look for
`certificate obtained successfully`.

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
`GET /v1/keys` (same header) lists them, without secrets.

To revoke a key, on the server:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
    -H "Authorization: Bearer $(sudo cat /var/lib/wirefan/admin.token)" \
    http://127.0.0.1:6060/v1/keys/<key-id>
# expect: 204 (404 if no key has that id)
```

Revocation takes effect at once: new connections with that key are
refused with `401`, and sockets already open under it are closed with
WebSocket close code `1008`, reason `key revoked`.

End-to-end pub/sub check from your laptop:

1. Open `https://wirefan.example.com/?key=<key-id>` in two browser tabs.
2. In each tab: connect, then subscribe to the demo channel.
3. Publish in tab A; tab B receives it.

The wire protocol for real clients is in `docs/PROTOCOL.md`.

---

## 7. Upgrade

Get the new binary and its checksum onto the server (step 4, with `VER`
set to the new tag), then:

```bash
sudo ~/deploy/deploy.sh ~/wirefan_${VER}_linux_${ARCH} ~/SHA256SUMS
```

(Option C builds have no `SHA256SUMS`; pass the digest instead:
`"$(sha256sum ~/wirefan_${VER}_linux_${ARCH} | awk '{print $1}')"`. With
an Option C checkout the script is `~/wirefan/deploy/deploy.sh`.)

`deploy.sh` verifies the SHA-256 (refusing on mismatch), stops the
service, snapshots the database, keeps the current binary at
`/usr/local/bin/wirefan.prev`, swaps in the new one, starts, and polls
`http://127.0.0.1:8080/v1/health` for up to 30 seconds. **If the health
check fails it automatically rolls back**: it restores the database
snapshot, puts the previous binary back, restarts, and exits non-zero.
The database is part of the rollback because a new version may migrate
the schema on its first start, and an older binary refuses to open a
database whose schema is newer than it knows.

The snapshot is `/var/lib/wirefan/wirefan.db.prev`, plus
`wirefan.db-wal.prev` and `wirefan.db-shm.prev` when those sidecars
existed at stop time. Each `deploy.sh` run replaces it. Otherwise state
carries over: the admin token and API keys stay in `/var/lib/wirefan`.
One behavior to know: subscribe tokens for `private-`/`presence-`
channels are signed with a per-process secret, so any restart invalidates
already-issued tokens; clients must fetch a fresh one from
`POST /v1/auth/sign` and reconnect.

To see which version is installed, run `/usr/local/bin/wirefan --version`.
Every start also logs it: `sudo journalctl -u wirefan | grep 'wirefan starting'`
shows lines ending in `version=v1.0.0 go=go1.26.x`.

### Manual rollback

If a problem surfaces after a green health check, first find out whether
the upgrade migrated the database schema (needs
`sudo apt-get install -y sqlite3`):

```bash
sudo sqlite3 /var/lib/wirefan/wirefan.db 'PRAGMA user_version'
sudo sqlite3 /var/lib/wirefan/wirefan.db.prev 'PRAGMA user_version'
```

**Same number:** the schema did not change and swapping the binary back
is enough. API keys minted since the upgrade are kept:

```bash
sudo ~/deploy/deploy.sh /usr/local/bin/wirefan.prev \
    "$(sha256sum /usr/local/bin/wirefan.prev | awk '{print $1}')"
```

This command both reads and rewrites `wirefan.prev` (the bad binary you
are rolling away from becomes the new `.prev`). It is safe because
`deploy.sh` copies the verified binary to a staging file before it
touches `wirefan.prev`.

**Different numbers:** the new version migrated the schema, so the
previous binary refuses the current database (it logs
`database schema version N is newer than this binary supports`) and the
database has to go back too. Do not use `deploy.sh` for this: it would
snapshot the migrated database over `wirefan.db.prev`, fail its health
check, and roll forward again. Restore by hand, as root (the state dir
is mode 0700):

```bash
sudo -i
systemctl stop wirefan
cd /var/lib/wirefan
mkdir -p /root/wirefan-migrated-db
for f in wirefan.db wirefan.db-wal wirefan.db-shm; do
    [ ! -f "$f" ] || cp -p "$f" /root/wirefan-migrated-db/
    if [ -f "$f.prev" ]; then cp -p "$f.prev" "$f"; else rm -f "$f"; fi
done
install -m 0755 /usr/local/bin/wirefan.prev /usr/local/bin/wirefan
systemctl start wirefan
curl -fsS http://127.0.0.1:8080/v1/health    # expect: ok
exit
```

This rolls back data, not just code. Every API key minted since the
upgrade is lost (its clients get `401` until you mint a replacement),
and every key revoked since the upgrade is valid again, so revoke those
once more. The migrated database stays in `/root/wirefan-migrated-db`
for reference.

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
sudo rm -f /var/lib/wirefan/wirefan.db-wal /var/lib/wirefan/wirefan.db-shm
sudo chown wirefan:wirefan /var/lib/wirefan/wirefan.db
sudo chmod 0600 /var/lib/wirefan/wirefan.db
sudo systemctl start wirefan
curl -fsS https://wirefan.example.com/v1/health
```

The `rm -f` of the `-wal`/`-shm` sidecars matters: the database runs in
WAL mode, and sidecars left over from the replaced database would be
replayed on top of the restored file.

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

The series wirefan defines (source of truth: `internal/metrics/prom.go`),
next to the standard `go_*` and `process_*` series of the Prometheus Go
client:

- `wirefan_connections` (gauge): open WebSocket connections right now.
- `wirefan_channels` (gauge): channels in the registry, read from the
  live registry at scrape time, so it cannot drift as channels are
  created lazily and reaped by the once-a-minute sweep. It includes the
  `_wirefan-stats` system channel, which the stats publisher creates on
  every 5-second tick if it is missing, whether or not anyone
  subscribes. An idle server therefore reads 1, except for up to 5
  seconds after each sweep has removed the unsubscribed stats channel.
- `wirefan_messages_published_total` (counter): messages accepted for
  fanout.
- `wirefan_messages_dropped_total{reason="slow_consumer"}` (counter):
  events not delivered because a subscriber's send buffer was full; that
  subscriber is then disconnected. Nonzero is the documented at-most-once
  behavior, not a bug. The series is absent until the first drop.
- `wirefan_broadcast_latency_seconds` (histogram): how long a publish
  spends in the fanout call. With the default `--fanout=per-conn` that is
  queueing the event on every subscriber's send buffer (not the network
  write). With `--fanout=sharded` it is only the handoff to a worker
  queue, so the per-subscriber work is not included.
- `wirefan_upgrade_rejected_total{reason}` (counter): refused WebSocket
  upgrades. Two reasons exist: `bad_key` (missing, unknown, or revoked
  key; the client gets `401`) and `phantom_cap` (the per-IP connection
  cap; `429`). Origin mismatches are not counted here: they get `403`
  and a `ws upgrade failed` warning in the log.
- `wirefan_auth_failures_total` (counter): failed subscribe-token checks
  on `private-`/`presence-` channels.

The read-only `_wirefan-stats` channel publishes a subset of these over
WebSocket every 5 seconds: `connections`, `channels`, `published`
(also sent as `messages_published_total`), and `dropped` (summed across
reasons). They are read from the same collectors, so they match a scrape
taken at the same moment.

For external scraping, do not open 6060 to the internet. Either run
Prometheus/Grafana Agent on the VPS itself, or reach the admin listener
over a private network: change `--admin-addr` in the unit from
`127.0.0.1:6060` to the VPS's WireGuard/Tailscale address and scrape
that. Only peers on that network can then connect, but they reach
`/v1/keys` (admin-token protected) and `/debug/pprof` as well, so treat
the network as trusted. A `provision.sh` re-run rewrites the unit, so
re-apply the change afterwards.

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
`--allowed-origins`. wirefan refuses to start insecurely: it logs a
single line and exits with status 1, and systemd (`Restart=on-failure`)
retries every 5 seconds, so the journal repeats that line and
`systemctl status` shows `activating (auto-restart)`. The line names the
problem in `err`:

```
ERROR fatal err="--allowed-origins is required (use --allowed-origins=https://your.host or pass --dev with --allowed-origins=*)"
```

Every other startup error (an unreadable state dir, a database from a
newer wirefan) produces the same `ERROR fatal err=...` shape. During
graceful shutdown `/v1/health` flips to `503` with body `draining` so
load balancers can drain; steady state is `200` `ok`.

---

## Appendix A: Docker instead of a binary

The systemd-plus-binary path above is the recommended one (smallest moving
parts, full unit hardening). A container path exists too:
`deploy/Dockerfile` builds a distroless image; `deploy/README.md` shows
how to run and smoke-test it. Two container-specific requirements:

- **Admin listener.** Pass `--admin-addr=0.0.0.0:6060` (loopback inside
  the container is unreachable through a port mapping) and publish it
  bound to the host's loopback only: `-p 127.0.0.1:6060:6060`.
- **State volume.** The admin token and key database live in
  `/var/lib/wirefan`. The image declares it a volume, but unless you name
  one, Docker gives each new container a fresh anonymous volume, so the
  token and keys are lost when the container is replaced. The image runs
  as distroless's `nonroot` user (uid 65532) and ships that directory
  owned by it. A named volume (`-v wirefan-state:/var/lib/wirefan`)
  copies that ownership when Docker creates it, so it works as is. A bind
  mount of a host directory keeps the host's ownership instead: create
  it and `sudo chown 65532:65532` it first. Otherwise wirefan exits at
  startup with SQLite's unhelpful wording for a directory it cannot
  write:
  `ERROR fatal err="open /var/lib/wirefan/wirefan.db: read schema version: unable to open database file: no such file or directory"`.

## Appendix B: no public IP? Cloudflare Tunnel

If you are running on a home machine behind NAT instead of a VPS,
Cloudflare Tunnel works: run `cloudflared` pointing
`wirefan.example.com` at `http://127.0.0.1:8080` (not `localhost`, which
may resolve to `::1` and then not match the trusted proxy), run wirefan with
`WIREFAN_TRUSTED_PROXIES=127.0.0.1` and
`--allowed-origins=https://wirefan.example.com`, and skip Caddy entirely
(Cloudflare terminates TLS at its edge). Tradeoffs: availability tracks
the home machine, and Cloudflare sees decrypted traffic.

## Appendix C: Cloudflare proxy in front (orange cloud)

The runbook's default is direct-to-origin: gray-cloud DNS, Caddy fetching
its own Let's Encrypt certificate, clients connecting straight to the VPS.
That stays the recommended simple path. This appendix is the alternative:
the DNS record set to **Proxied (orange cloud)**, so every client
connection passes through Cloudflare's network first. What you gain: DDoS
absorption at Cloudflare's edge, the WAF, and an origin IP that no longer
appears in public DNS. What changes: three things below, in order of how
badly they hurt when missed. Cloudflare sees decrypted traffic in this
topology (it terminates TLS at its edge before re-encrypting to the
origin); if that is unacceptable, stay on the direct path.

### C.1 Trusted proxies (skip this and you get an outage)

With the orange cloud on, **every** TCP connection reaching the origin
comes from a Cloudflare edge address, not from the client. wirefan caps
concurrent connections per client IP (`WIREFAN_IP_CAP`, default 200), and
it attributes a connection to the `X-Forwarded-For` client IP **only**
when the directly connected peer is listed in `WIREFAN_TRUSTED_PROXIES`.
The provision script sets that variable to `127.0.0.1` because Caddy
proxies from loopback. Out of the box Caddy trusts no upstream proxy, so
it discards the `X-Forwarded-For` it receives and sends wirefan the
address of whoever connected to it, which is now a Cloudflare edge. The
net effect: all of your users collapse into a handful of Cloudflare IPs,
the per-IP cap fills up, and connection 201 is refused no matter who it
is. Traffic ramps, then legitimate users start getting rejected, and
nothing in the wirefan logs says "Cloudflare" anywhere.

The fix is to trust the Cloudflare ranges in **both** layers:

1. In `/etc/wirefan/env`, set
   `WIREFAN_TRUSTED_PROXIES=127.0.0.1,<cloudflare-ipv4-cidrs>,<cloudflare-ipv6-cidrs>`
   (comma-separated).
2. In the Caddyfile, add the same ranges as `trusted_proxies` inside
   `reverse_proxy` (C.2 shows the block). Caddy then keeps the
   `X-Forwarded-For` chain Cloudflare sent and appends the edge address,
   instead of replacing the chain with the edge address.

How the client address travels, and why each layer is needed. wirefan
walks `X-Forwarded-For` from the right and takes the first hop that is
not in `WIREFAN_TRUSTED_PROXIES`, and only when the connection itself
comes from a trusted address (here always Caddy, `127.0.0.1`):

| Topology | `WIREFAN_TRUSTED_PROXIES` | Caddy `trusted_proxies` | `X-Forwarded-For` reaching wirefan | wirefan picks |
|---|---|---|---|---|
| Direct (gray cloud) | `127.0.0.1` | none (shipped Caddyfile) | `<client>` | `<client>` |
| Cloudflare (orange cloud) | `127.0.0.1,<cloudflare ranges>` | `<cloudflare ranges>` | `<client>, <edge>` | `<client>` (`<edge>` is trusted, skipped) |
| Cloudflare, only wirefan updated | `127.0.0.1,<cloudflare ranges>` | none | `<edge>` | `<edge>` (every hop trusted, falls back to it) |
| Cloudflare, only Caddy updated | `127.0.0.1` | `<cloudflare ranges>` | `<client>, <edge>` | `<edge>` (first untrusted hop) |

Spoofing does not get through either correct setup. Direct: Caddy drops
whatever `X-Forwarded-For` a client sends. Cloudflare: Cloudflare appends
the address that connected to it, so anything a client prepends ends up
left of the real `<client>` hop, which wirefan reaches first.

The ranges themselves are published at <https://www.cloudflare.com/ips/>
(machine-readable at `https://www.cloudflare.com/ips-v4/` and
`https://www.cloudflare.com/ips-v6/`). They are deliberately not
reproduced here: a hardcoded copy in a runbook rots, and a stale list
silently reintroduces the exact failure described above for whatever
slice of traffic arrives via a newer range. Fetch them at setup time:

```bash
CF_V4=$(curl -fsS https://www.cloudflare.com/ips-v4/ | paste -sd, -)
CF_V6=$(curl -fsS https://www.cloudflare.com/ips-v6/ | paste -sd, -)
echo "WIREFAN_TRUSTED_PROXIES=127.0.0.1,${CF_V4},${CF_V6}"
# paste that line into /etc/wirefan/env, then:
sudo systemctl restart wirefan
# the same ranges, space-separated, for the Caddyfile (C.2):
echo "trusted_proxies $(echo "${CF_V4},${CF_V6}" | tr ',' ' ')"
```

**The list must be refreshed.** Cloudflare changes it rarely but does
change it. Re-run the fetch on a schedule (a monthly cron that regenerates
the line and the Caddyfile's `trusted_proxies`, then restarts wirefan and
reloads Caddy, is enough) or whenever Cloudflare announces a range change.
Note the footgun in `.env.example`: malformed entries are silently
dropped, so smoke-test after every edit by connecting and checking that
`wirefan_upgrade_rejected_total{reason="phantom_cap"}` (the per-IP cap)
is not climbing with real traffic.

### C.2 TLS: origin certificate, Full (Strict), no ACME

Once the proxy is on, HTTP-01 ACME on the origin gets awkward: the
challenge traffic arrives through Cloudflare, and certificate renewal now
depends on the proxy behaving. The clean answer is to stop doing ACME on
the origin entirely and use a **Cloudflare Origin Certificate**: free,
issued in the dashboard (SSL/TLS, then Origin Server), valid for up to 15
years, and trusted by Cloudflare's edge (only by Cloudflare, which is fine
because Cloudflare is now the only thing connecting to 443).

Set the zone's SSL/TLS mode to **Full (Strict)**: Cloudflare connects to
the origin over TLS and validates the certificate.

**Do not use Flexible mode.** Flexible makes Cloudflare talk plain HTTP
to the origin: the browser sees HTTPS but the Cloudflare-to-origin leg is
cleartext across the public internet, which defeats the point, and the
origin receives `http://` traffic on a listener expecting TLS, which
breaks `wss://` upgrades. If clients can reach the site over HTTPS but
WebSocket connections fail or the origin sees plaintext on 443, check the
SSL/TLS mode first.

Install the origin certificate and key on the server, then switch Caddy
from ACME to serving the provided pair. Replace the site block that
`provision.sh` wrote in `/etc/caddy/Caddyfile` with:

```caddyfile
wirefan.example.com {
    tls /etc/caddy/cf-origin.pem /etc/caddy/cf-origin.key
    encode gzip

    reverse_proxy 127.0.0.1:8080 {
        trusted_proxies <cloudflare-ranges>   # the trusted_proxies line printed in C.1
        flush_interval -1
    }

    @internal path /debug/pprof*
    respond @internal "Not Found" 404
}
```

Two lines differ from the shipped Caddyfile. `tls <cert> <key>`: with it
present, Caddy serves that certificate instead of requesting one.
`trusted_proxies`: C.1. Do not add `header_up X-Forwarded-For ...` or
similar; overwriting the header throws away the client address
Cloudflare sent. Caddy runs as the `caddy` user, so make the key
readable by that group and nobody else
(`sudo chown root:caddy /etc/caddy/cf-origin.key && sudo chmod 0640 /etc/caddy/cf-origin.key`),
then reload with `sudo systemctl reload caddy`. Remember that re-running
`provision.sh` rewrites the Caddyfile, so re-apply this block (the
previous version is saved as a `.bak`).

### C.3 Firewall: 443 accepts only Cloudflare

Hiding the origin IP from DNS is cosmetic on its own: the IP leaks through
old DNS history, certificate transparency logs, or plain scanning, and an
attacker who finds it can bypass Cloudflare entirely and hit the origin
direct. Origin hiding becomes real when the host firewall refuses 443 from
anywhere that is not Cloudflare:

```bash
# remove the open-to-the-world rules from step 3
sudo ufw delete allow 80/tcp
sudo ufw delete allow 443/tcp

# allow 443 only from Cloudflare's published ranges
for r in $(curl -fsS https://www.cloudflare.com/ips-v4/) \
         $(curl -fsS https://www.cloudflare.com/ips-v6/); do
    sudo ufw allow proto tcp from "$r" to any port 443
done
sudo ufw status numbered
```

Port 80 can stay closed: it existed for the HTTP-01 challenge, and C.2
removed ACME. Port 22 stays open as before (or restrict it to your own
IP, which is unrelated to Cloudflare). Mirror the same restriction in the
provider firewall if it supports source ranges.

The maintenance cost is the same as C.1, doubled: the ufw rules pin
today's Cloudflare ranges, and when Cloudflare adds a range, traffic from
it is dropped at the firewall before wirefan ever sees it. Refresh the
rules on the same schedule as the env variable, and treat "some users
suddenly cannot connect at all" as a prompt to diff the live list at
<https://www.cloudflare.com/ips/> against `ufw status`.

### C.4 Why WebSockets survive the proxy

Cloudflare proxies WebSocket connections but reaps ones that sit idle at
its edge; Cloudflare documents the proxy idle timeout as being on the
order of 100 seconds. wirefan never lets a healthy connection go idle
that long: the server sends a WebSocket ping on every open connection
every 30 seconds (`pingInterval` in `internal/conn/conn.go`) and drops a
peer that does not answer within 10 seconds. Browsers and WebSocket
libraries answer pings on their own, so a healthy client that is only
listening stays connected, the proxy sees traffic in both directions
every 30 seconds, and a dead peer is cleaned up within about 40 seconds.
No Cloudflare timeout tuning, keepalive configuration, or client-side
heartbeat is needed. If you ever change `pingInterval`, keep it
comfortably under Cloudflare's idle timeout or proxied connections will
start dying quietly during quiet periods.

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
