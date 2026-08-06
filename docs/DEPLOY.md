# wirefan deployment runbook

This is the linear, copy-pasteable runbook for getting wirefan onto a public
URL on a $0 host. Target setup: **Oracle Cloud Always Free Ampere A1 (ARM64)**
+ Caddy + Let's Encrypt + Cloudflare DNS, with a **Cloudflare Tunnel** fallback
documented at the end if Oracle capacity is unavailable.

The host targeted in commands: `wirefan.ethanyucetepe.dev`. Substitute your own
hostname throughout if forking.

Three facts the whole runbook leans on:

- **`--allowed-origins` is required.** wirefan refuses to start without it,
  and refuses `*` outside `--dev`. Every invocation below passes it.
- **The admin token is a file, not a log line.** On first boot wirefan writes
  a token to `$WIREFAN_STATE_DIR/admin.token` (mode 0600) and reuses it on
  every later boot. It is never printed. You read the file, or you set
  `WIREFAN_ADMIN_TOKEN` yourself.
- **The admin listener is separate from the public one.** `/v1/keys`,
  `/metrics`, and `/debug/pprof/*` live on `--admin-addr` (default
  `127.0.0.1:6060`), which is never exposed through Caddy. Key minting and
  scraping happen from the host.

---

## 0. Prerequisites

- **Oracle Cloud account** with Always Free tier active. CC required for
  signup, never charged on Always-Free shapes.
- **GitHub PAT** scoped `write:packages` + `read:packages` for pushing to
  `ghcr.io/ethany33/wirefan`. Create at <https://github.com/settings/tokens>.
- **Domain on Cloudflare DNS** (this runbook uses `ethanyucetepe.dev`).
- **Local Docker with buildx** for cross-arch builds: `docker buildx version`.
- **SSH keypair**: `ssh-keygen -t ed25519 -f ~/.ssh/wirefan_oracle`.
- **The wirefan repo checked out locally** so you can reference `deploy/`.

---

## 1. Provision the Oracle Ampere A1 instance

1. Sign in at <https://cloud.oracle.com>. Pick your home region.
2. Hamburger menu -> **Compute** -> **Instances** -> **Create Instance**.
3. Name: `wirefan`.
4. **Image and shape** -> **Edit**:
   - Image: **Canonical Ubuntu 22.04** (LTS), make sure it is the **aarch64**
     variant.
   - Shape: **Ampere** -> **VM.Standard.A1.Flex**. Start with **1 OCPU / 6 GB**;
     the Always-Free pool gives you up to **4 OCPU / 24 GB total** across all
     A1 instances, so you can scale this one up later without leaving the free
     tier.
5. **Networking**: accept the default VCN + public subnet, or create a new VCN
   with the wizard. Make sure **Assign a public IPv4 address** is checked.
6. **Add SSH keys** -> **Paste public keys** -> paste the contents of
   `~/.ssh/wirefan_oracle.pub`.
7. **Boot volume**: the default 47 GB is fine and within the Always-Free 200 GB
   total block-volume budget.
8. Click **Create**.

> **Trap: A1 capacity.** Always-Free Ampere capacity is constrained. If
> creation fails with `Out of host capacity`: retry (capacity rotates
> throughout the day), try a different availability domain in the same
> region, or script a retry loop with the OCI CLI. If capacity stays
> unavailable for 24+ hours, jump to **Path B: Cloudflare Tunnel** below.

Once the instance is **Running**, copy the **Public IP** from the instance
detail page — you'll use it as `<public-ip>` below.

---

## 2. Initial server setup

SSH in using the keypair you uploaded:

```bash
ssh -i ~/.ssh/wirefan_oracle ubuntu@<public-ip>
```

Then on the host:

```bash
sudo apt-get update && sudo apt-get upgrade -y
sudo apt-get install -y docker.io ca-certificates curl gnupg
sudo systemctl enable --now docker
sudo usermod -aG docker ubuntu
# log out + back in for the docker group membership to take effect
exit
```

Reconnect and verify Docker works without sudo:

```bash
ssh -i ~/.ssh/wirefan_oracle ubuntu@<public-ip>
docker version
docker run --rm hello-world
```

---

## 3. Build and push the image

From your **local dev box** (x86_64), cross-build for ARM64 and push to GHCR.

First-time GHCR login (uses your PAT as the password):

```bash
docker login ghcr.io -u EthanY33
# password prompt: paste the PAT (write:packages scope)
```

Then build + push:

```bash
docker buildx create --use --name wirefan-builder
docker buildx inspect --bootstrap
docker buildx build \
    --platform linux/arm64 \
    -t ghcr.io/ethany33/wirefan:latest \
    --push \
    -f deploy/Dockerfile .
```

> **Trap: image privacy.** New GHCR images default to private. After the first
> push, go to <https://github.com/users/EthanY33/packages/container/wirefan>
> and either set the package public (so you don't need to log in on the
> server) or keep it private and `docker login` on the Oracle host too.

Tag a versioned image too so rollback has something to roll back to:

```bash
GIT_SHA=$(git rev-parse --short HEAD)
docker buildx build \
    --platform linux/arm64 \
    -t ghcr.io/ethany33/wirefan:latest \
    -t ghcr.io/ethany33/wirefan:$GIT_SHA \
    --push \
    -f deploy/Dockerfile .
```

---

## 4. Pull and run on the Oracle instance

SSH back in to the Oracle host. From here on commands run on the **server**
unless noted otherwise.

```bash
sudo docker pull ghcr.io/ethany33/wirefan:latest
```

Create the config dir and the state dir. The state dir holds `admin.token`
and `wirefan.db`, so it is the one path that must survive redeploys:

```bash
sudo mkdir -p /etc/wirefan /var/lib/wirefan
```

Copy the deploy templates from the repo. Easiest way is to clone the repo on
the host (read-only) just to grab the `deploy/` files:

```bash
sudo apt-get install -y git
git clone https://github.com/EthanY33/wirefan.git /tmp/wirefan-src
sudo cp /tmp/wirefan-src/deploy/.env.example /etc/wirefan/env
sudo cp /tmp/wirefan-src/deploy/Caddyfile /etc/caddy/Caddyfile  # used in step 5
sudo cp /tmp/wirefan-src/deploy/wirefan.service /etc/systemd/system/wirefan.service
sudo chmod 0600 /etc/wirefan/env
```

Edit `/etc/wirefan/env`:

```bash
sudo nano /etc/wirefan/env
```

The template documents every variable. The two that matter here:

- `WIREFAN_TRUSTED_PROXIES` — **required behind Caddy.** wirefan caps
  concurrent connections per client IP (200 by default). With this unset,
  every connection through Caddy is attributed to Caddy's IP, so the per-IP
  cap silently becomes a global 200-connection ceiling. Set it to
  `172.17.0.0/16` for the Docker mode below (Caddy reaches the container
  through the default bridge), or `127.0.0.1` for the native-binary mode.
- `WIREFAN_ADMIN_TOKEN` — optional. Leave it unset and wirefan generates a
  token on first boot, persists it at `/var/lib/wirefan/admin.token`, and
  reuses it forever after. Set it only if you want to pick the value.

### 4a. Pick a deploy mode for systemd

The shipped `deploy/wirefan.service` runs `/usr/local/bin/wirefan` directly
(binary-on-disk). With Docker, you have two equally valid options.

**Option A — Run the container under systemd (recommended for this runbook).**

The container runs as distroless `nonroot` (uid 65532), so the bind-mounted
state dir must be writable by that uid:

```bash
sudo chown -R 65532:65532 /var/lib/wirefan
```

Override `ExecStart` so systemd manages the container's lifecycle:

```bash
sudo systemctl edit wirefan
```

Paste (replace the origin with your hostname):

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/docker run --rm --name wirefan \
    -p 8080:8080 \
    -p 127.0.0.1:6060:6060 \
    --env-file /etc/wirefan/env \
    -v /var/lib/wirefan:/var/lib/wirefan \
    ghcr.io/ethany33/wirefan:latest \
    --listen=:8080 \
    --admin-addr=0.0.0.0:6060 \
    --allowed-origins=https://wirefan.ethanyucetepe.dev \
    --db-path=/var/lib/wirefan/wirefan.db
ExecStop=/usr/bin/docker stop -t 30 wirefan
# The shipped unit's hardening (ProtectSystem=strict, etc.) is designed for a
# native binary; clear those so `docker run` works. Empty assignments unset.
ProtectSystem=
ProtectHome=
PrivateTmp=
PrivateDevices=
ProtectKernelTunables=
ProtectKernelModules=
ProtectControlGroups=
MemoryDenyWriteExecute=
NoNewPrivileges=
User=
Group=
ReadWritePaths=
WorkingDirectory=
```

The empty `ExecStart=` line **first** is required by systemd to clear the
shipped value before adding ours.

Two flag notes:

- `--admin-addr=0.0.0.0:6060` is required in Docker: bound to container
  loopback, the admin listener would be unreachable through the port
  mapping. The `-p 127.0.0.1:6060:6060` publish keeps it host-loopback-only,
  so it is still not internet-reachable.
- `--db-path` is explicit here for clarity; it is also the default
  (`$WIREFAN_STATE_DIR/wirefan.db`, and the image sets
  `WIREFAN_STATE_DIR=/var/lib/wirefan`).

**Option B — Extract the binary from the image and put it at
`/usr/local/bin/wirefan`.** This keeps every `wirefan.service` hardening flag
and gives you a smaller process tree. Tradeoff: you re-extract on every
update.

```bash
# Pull or use the existing image
CID=$(sudo docker create ghcr.io/ethany33/wirefan:latest)
sudo docker cp $CID:/wirefan /usr/local/bin/wirefan
sudo docker rm $CID
sudo chmod +x /usr/local/bin/wirefan

# The shipped unit runs as the wirefan user; create it and hand it the state dir
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wirefan || true
sudo chown -R wirefan:wirefan /var/lib/wirefan

# Edit the unit's --allowed-origins placeholder to your hostname
sudo nano /etc/systemd/system/wirefan.service
```

Either way, finish with:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now wirefan
sudo systemctl status wirefan
```

You should see `active (running)`. If not, jump to **Logs** in the operational
runbook section. The most common first-boot failure is a missing or invalid
`--allowed-origins`: wirefan exits immediately with a usage error rather than
starting insecurely.

---

## 5. Caddy install + config

```bash
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy
```

The Caddyfile was already copied in step 4. If you didn't copy it then, do
it now:

```bash
sudo cp /tmp/wirefan-src/deploy/Caddyfile /etc/caddy/Caddyfile
```

Reload Caddy to pick up the config:

```bash
sudo systemctl reload caddy
sudo systemctl status caddy
```

Caddy will try to fetch a Let's Encrypt cert immediately — but it will fail
until DNS is in place (next step) and Oracle ingress lets 80/443 through
(step 7). Errors at this stage are fine; Caddy retries with backoff.

> **Reminder:** Caddy forwards `X-Forwarded-For`, but wirefan ignores it
> unless the proxy's address is listed in `WIREFAN_TRUSTED_PROXIES` (step 4).
> If you skipped that, the per-IP connection cap is now a global cap.

---

## 6. DNS

In Cloudflare DNS for `ethanyucetepe.dev`:

1. **Add record** -> Type **A**.
2. Name: `wirefan`.
3. IPv4: `<public-ip>` of the Oracle instance.
4. Proxy status: **DNS only (gray cloud)**. **Do not enable orange-cloud
   proxying.** Caddy needs to terminate TLS itself for HTTP/01 ACME, and the
   orange cloud both intercepts that handshake and breaks websockets unless
   you reconfigure to TLS-on-the-edge.
5. TTL: Auto.
6. Save.

Wait for propagation (usually 30–60s on Cloudflare):

```bash
dig +short wirefan.ethanyucetepe.dev
# expect: <public-ip>
```

---

## 7. Oracle ingress security list

By default the Oracle VCN security list **blocks 80 and 443**. Open them:

1. Console -> **Networking** -> **Virtual Cloud Networks** -> click your VCN.
2. **Security Lists** -> click the default security list.
3. **Add Ingress Rules**:
   | Source CIDR | IP Protocol | Source Port | Destination Port |
   |---|---|---|---|
   | 0.0.0.0/0   | TCP | All | 80  |
   | 0.0.0.0/0   | TCP | All | 443 |
   | 0.0.0.0/0   | TCP | All | 22  |
4. Save.

> **Optional hardening:** lock 22 to your home IP only (`<your-ip>/32`). You
> can always loosen it later if your IP rotates.

Now Caddy's next ACME attempt should succeed; check:

```bash
sudo journalctl -u caddy -n 50 --no-pager
```

You want to see lines like `obtained certificate ... wirefan.ethanyucetepe.dev`.

---

## 8. Smoke test

From your laptop:

```bash
curl -i https://wirefan.ethanyucetepe.dev/v1/health
# expected: HTTP/1.1 200 OK, plain-text body: ok

curl -i https://wirefan.ethanyucetepe.dev/
# expected: HTTP/1.1 200 OK + the demo HTML page
```

If TLS isn't working yet but `:80` is, you'll get a 308 redirect to https
(Caddy default). If you get connection-refused, ingress (step 7) is still
blocking.

The end-to-end pub/sub check needs an API key, so it comes after step 9.

---

## 9. Read the admin token + mint API keys

The admin token is **never printed**. On first boot wirefan writes it to
`$WIREFAN_STATE_DIR/admin.token` with mode 0600 and reuses that file on every
later boot. Both deploy modes above put the state dir at `/var/lib/wirefan`
(for Docker it is a bind mount, so the file is visible on the host):

```bash
sudo cat /var/lib/wirefan/admin.token
```

If you set `WIREFAN_ADMIN_TOKEN` in `/etc/wirefan/env`, that value is the
token and no file is consulted.

Key minting happens on the **admin listener**, which both deploy modes keep
on host loopback (`127.0.0.1:6060`). It is deliberately not reachable through
Caddy, so mint from the server itself:

```bash
curl -s -X POST http://127.0.0.1:6060/v1/keys \
    -H "Authorization: Bearer $(sudo cat /var/lib/wirefan/admin.token)" \
    -H "Content-Type: application/json" \
    -d '{"name":"production-app"}'
```

The response includes the key `id` and a `secret` shown **once**. Save the
secret in your app server's config — you cannot recover it later. Keys are
stored in SQLite at `/var/lib/wirefan/wirefan.db` and survive restarts.

Now the end-to-end pub/sub check:

1. Open `https://wirefan.ethanyucetepe.dev/?key=<key-id>` in two browser tabs.
2. Both tabs connect and subscribe to the demo channel.
3. Publish a message in tab A -> tab B receives it.

---

## 10. Monitoring

wirefan exposes Prometheus metrics on the **admin listener**, not the public
one:

```bash
# from the host
curl http://127.0.0.1:6060/metrics | head
```

You have two reasonable options for external scraping:

1. **Add a second Caddy site** for `metrics.wirefan.ethanyucetepe.dev`,
   protected by basic-auth or IP allowlist, that proxies to
   `127.0.0.1:6060/metrics`. Quick, but exposes the metrics surface to the
   internet.
2. **Tailscale tunnel.** Install Tailscale on the Oracle host, scrape from a
   Prometheus running on your home network. No public exposure, no auth to
   manage. Recommended.

Either way, add scrape config to your Prometheus pointing at the metrics
endpoint. The wirefan histograms are documented in `docs/DESIGN.md`.

---

## 11. Benchmarks

Published numbers and the reproduction methodology live in
`docs/BENCHMARKS.md`; the reproduction commands there run the benchmark in a
CPU- and memory-constrained local container, not against this host. If you
want to sanity-check the deployed instance, `cmd/loadtest` can point at any
reachable wirefan; mind that the per-key rate limit (100 msg/s, burst 200)
and the per-IP cap both shape what a single-key, single-source run can show.
`scripts/bench.sh` documents the flags a clean run needs.

---

## 12. GitHub social preview

Rasterize the social card and upload it:

```bash
# from a machine with rsvg-convert or Inkscape
rsvg-convert -w 1280 -h 640 docs/og-card.svg -o docs/og-card.png
```

Then go to GitHub repo -> **Settings** -> **General** -> **Social preview** ->
**Upload an image** -> pick `docs/og-card.png`.

---

## 13. Path B: Cloudflare Tunnel from a home machine

If Oracle Always-Free A1 capacity stays unavailable for an extended period
(24+ hours of retries, multiple ADs), fall back to running wirefan on an
always-on home machine (Raspberry Pi, NAS, mini-PC, desktop) and exposing it
via Cloudflare Tunnel.

Tradeoffs:

- **Pros:** zero Oracle dependency; no Caddy/systemd/security-list dance;
  Cloudflare handles TLS automatically; works from behind home NAT with no
  port forwarding.
- **Cons:** demo availability tracks home-machine uptime; bandwidth charged
  against your home connection; Cloudflare sees decrypted traffic.

### Steps

1. **Install cloudflared on the home machine.** From
   <https://github.com/cloudflare/cloudflared/releases/latest> grab the
   appropriate binary. On Debian/Ubuntu:
   ```bash
   curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb -o cloudflared.deb
   sudo dpkg -i cloudflared.deb
   ```
2. **Authenticate.** This opens a browser; pick your domain.
   ```bash
   cloudflared tunnel login
   ```
3. **Create the tunnel.**
   ```bash
   cloudflared tunnel create wirefan
   # outputs a UUID and credentials JSON path
   ```
4. **Map the hostname.** Either via `cloudflared tunnel route dns` or in the
   Cloudflare Zero Trust dashboard:
   ```bash
   cloudflared tunnel route dns wirefan wirefan.ethanyucetepe.dev
   ```
5. **Configure the tunnel** at `~/.cloudflared/config.yml`:
   ```yaml
   tunnel: <UUID>
   credentials-file: /home/<you>/.cloudflared/<UUID>.json
   ingress:
     - hostname: wirefan.ethanyucetepe.dev
       service: http://localhost:8080
     - service: http_status:404
   ```
6. **Run wirefan locally.** From the repo root on the home machine:
   ```bash
   go build -o bin/wirefan ./cmd/wirefan
   WIREFAN_TRUSTED_PROXIES=127.0.0.1 ./bin/wirefan \
       --listen=:8080 \
       --admin-addr=127.0.0.1:6060 \
       --allowed-origins=https://wirefan.ethanyucetepe.dev
   ```
   (cloudflared connects from localhost, so it is the "proxy" the per-IP cap
   must trust. Use the same systemd unit if you want it managed; the state
   dir defaults to `./var` when `WIREFAN_STATE_DIR` is unset.)
7. **Run the tunnel.**
   ```bash
   cloudflared tunnel run wirefan
   ```
   Or install as a service:
   ```bash
   sudo cloudflared service install
   ```
8. **Smoke test** the same way as step 8 (admin token at `./var/admin.token`
   in this mode, key minting still at `http://127.0.0.1:6060/v1/keys`).

The DNS record Cloudflare creates is automatically proxied (orange cloud) for
this path — that's correct for tunnels because Cloudflare itself terminates
TLS at the edge before forwarding through the tunnel.

---

## 14. Operational runbook

Day-to-day commands once the host is up.

### Restart

```bash
sudo systemctl restart wirefan
```

Restarts are cheap: the admin token and API keys live in
`/var/lib/wirefan`, so both survive. Subscribe tokens are short-lived and
signed with a per-process secret, so clients re-request them after a restart
(the demo client does this automatically).

### Logs

```bash
sudo journalctl -u wirefan -f          # follow
sudo journalctl -u wirefan -n 200      # last 200 lines
sudo journalctl -u wirefan --since "10 min ago"
```

For Docker mode:

```bash
sudo docker logs -f wirefan
```

(There is no admin token in these logs, by design. It is a file; see step 9.)

### Update to a new image

```bash
# from dev box: build + push :latest and :<sha> as in step 3
# on the server:
sudo docker pull ghcr.io/ethany33/wirefan:latest
sudo systemctl restart wirefan
sudo systemctl status wirefan
```

For Option B (binary on disk), re-run the docker-cp dance from step 4a then
restart.

### Backup

The state dir `/var/lib/wirefan` holds the two files that matter:
`wirefan.db` (SQLite, the API keys) and `admin.token`. Back up the db with
rsync on a cron:

```bash
# on a backup machine
rsync -avz \
    -e "ssh -i ~/.ssh/wirefan_oracle" \
    ubuntu@<public-ip>:/var/lib/wirefan/wirefan.db \
    ./backups/wirefan-$(date +%Y%m%d).db
```

SQLite supports the [online backup API](https://www.sqlite.org/backup.html);
for a stronger guarantee against in-flight writes:

```bash
sudo sqlite3 /var/lib/wirefan/wirefan.db ".backup /tmp/wirefan-snapshot.db"
```

### Drain check

The `/v1/health` endpoint (public listener) flips to `503` with body
`draining` during graceful shutdown so load balancers can drain; in steady
state it returns `200` with body `ok`:

```bash
curl -i http://localhost:8080/v1/health
```

### Process resource usage

```bash
sudo docker stats wirefan          # docker mode
top -p $(pgrep -f wirefan)         # native mode
```

If memory grows unboundedly, capture a heap profile from the admin listener:

```bash
curl -s http://127.0.0.1:6060/debug/pprof/heap > /tmp/heap.pprof
go tool pprof -top /tmp/heap.pprof
```

---

## 15. Cost monitoring

Always-Free Ampere A1 is permanently free at the spec'd shape. Sanity-check
every month or two:

1. Console -> **Billing & Cost Management** -> **Cost Analysis**.
2. Filter to current month.
3. Expected: **$0.00**.

If you see a non-zero estimate:

- Confirm the instance is still `VM.Standard.A1.Flex` and within
  4 OCPU / 24 GB total.
- Confirm boot volume is under the Always-Free 200 GB total block storage.
- Confirm you haven't accidentally provisioned a second public IP (only one
  reserved/ephemeral public IPv4 is free per region).
- Confirm egress has stayed under 10 TB/month (very unlikely to hit).

Set a budget alert: **Billing** -> **Budgets** -> create a $1 budget on the
compartment so you get an email if anything ever charges.

---

## 16. Outage / rollback

If a deploy goes bad:

```bash
# pull a known-good previous image
sudo docker pull ghcr.io/ethany33/wirefan:<previous-sha>

# point the unit at the old tag
sudo systemctl edit wirefan
# change ghcr.io/ethany33/wirefan:latest -> :<previous-sha>

sudo systemctl daemon-reload
sudo systemctl restart wirefan
sudo systemctl status wirefan
```

For data corruption: SQLite is a single file, so restore is
"stop wirefan, copy backup over `/var/lib/wirefan/wirefan.db`, start wirefan."

```bash
sudo systemctl stop wirefan
sudo cp ./backups/wirefan-20260501.db /var/lib/wirefan/wirefan.db
sudo chown 65532:65532 /var/lib/wirefan/wirefan.db   # docker mode
# sudo chown wirefan:wirefan /var/lib/wirefan/wirefan.db   # native mode
sudo systemctl start wirefan
```

For full host loss: re-run steps 1-9 against a fresh A1 instance. The SQLite
backup contains the keys; everything else is rebuilt from `deploy/`.

---

## Appendix: verify the image locally

Run this on any box with Docker before pushing anywhere:

```bash
docker build -t wirefan:dev -f deploy/Dockerfile .

docker run --rm --name wirefan-smoke \
    -p 8080:8080 -p 127.0.0.1:6060:6060 \
    wirefan:dev \
    --listen=:8080 --admin-addr=0.0.0.0:6060 --dev --allowed-origins='*'
```

In another shell:

```bash
curl -i http://localhost:8080/v1/health    # 200, body "ok"

# The admin token is a file inside the container; distroless has no shell,
# so copy it out:
docker cp wirefan-smoke:/var/lib/wirefan/admin.token ./admin.token
curl -s -X POST http://127.0.0.1:6060/v1/keys \
    -H "Authorization: Bearer $(cat admin.token)" \
    -d '{"name":"smoke"}'
# expect: {"id":"...","name":"smoke","secret":"..."}
```

The db file is created at `/var/lib/wirefan/wirefan.db` inside the container
(verify with `docker cp wirefan-smoke:/var/lib/wirefan/wirefan.db /tmp/x.db`).
Without a volume mount both files vanish with the container; that is fine for
a smoke test and exactly why production mounts `/var/lib/wirefan`.

---

## Cross-references

- `deploy/Dockerfile` — multi-stage build, distroless runtime, cgo for
  sqlite, nonroot-writable state dir at `/var/lib/wirefan`.
- `deploy/Caddyfile` — reverse-proxy + auto-TLS config.
- `deploy/wirefan.service` — systemd unit (binary-on-disk by default).
- `deploy/.env.example` — template for `/etc/wirefan/env`, documents
  `WIREFAN_TRUSTED_PROXIES`, `WIREFAN_STATE_DIR`, `WIREFAN_IP_CAP`,
  `WIREFAN_ADMIN_TOKEN`.
- `deploy/README.md` — short orientation to the deploy/ directory.
- `docs/DESIGN.md` — runtime architecture, fanout, backpressure.
- `docs/PROTOCOL.md` — wire protocol clients implement.
- `docs/BENCHMARKS.md` — benchmark methodology and published numbers.
