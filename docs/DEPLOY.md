# wirefan deployment runbook

This is the linear, copy-pasteable runbook for getting wirefan onto a public
URL on a $0 host. Target setup: **Oracle Cloud Always Free Ampere A1 (ARM64)**
+ Caddy + Let's Encrypt + Cloudflare DNS, with a **Cloudflare Tunnel** fallback
documented at the end if Oracle capacity is unavailable.

The host targeted in commands: `wirefan.ethanyucetepe.dev`. Substitute your own
hostname throughout if forking.

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

Create the system user, config dir, and data dir:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wirefan || true
sudo mkdir -p /etc/wirefan /var/lib/wirefan
sudo chown -R wirefan:wirefan /var/lib/wirefan
```

Copy the env template from the repo. Easiest way is to clone the repo on the
host (read-only) just to grab the deploy/ files:

```bash
sudo apt-get install -y git
git clone https://github.com/EthanY33/wirefan.git /tmp/wirefan-src
sudo cp /tmp/wirefan-src/deploy/.env.example /etc/wirefan/env
sudo cp /tmp/wirefan-src/deploy/Caddyfile /etc/caddy/Caddyfile  # used in step 5
sudo cp /tmp/wirefan-src/deploy/wirefan.service /etc/systemd/system/wirefan.service
sudo chmod 0600 /etc/wirefan/env
sudo chown wirefan:wirefan /etc/wirefan/env
```

Edit `/etc/wirefan/env` to taste:

```bash
sudo nano /etc/wirefan/env
```

Settings to consider uncommenting:

- `WIREFAN_ADMIN_TOKEN=` — set a stable admin Bearer so it survives restarts.
- `WIREFAN_SIGNING_SECRET=` — set a stable HMAC secret so already-issued
  channel tokens stay valid across restarts.
- `OTEL_EXPORTER_OTLP_ENDPOINT=` — leave empty unless you have an OTel
  collector to point at.

> **Honest gotcha.** Reading those env vars in `cmd/wirefan/main.go` is a
> deferred task — the unit file sources `EnvironmentFile=-/etc/wirefan/env`
> and the Docker option below uses `--env-file`, so the values reach the
> process, but until the wiring lands wirefan still mints fresh secrets at
> boot. Until then, expect tokens to invalidate on every restart and pull
> the admin Bearer from journal each time (see step 9).

### 4a. Pick a deploy mode for systemd

The shipped `deploy/wirefan.service` runs `/usr/local/bin/wirefan` directly
(binary-on-disk). With Docker, you have two equally valid options.

**Option A — Run the container under systemd (recommended for this runbook).**

Override `ExecStart` so systemd manages the container's lifecycle:

```bash
sudo systemctl edit wirefan
```

Paste:

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/docker run --rm --name wirefan \
    -p 8080:8080 \
    --env-file /etc/wirefan/env \
    -v /var/lib/wirefan:/var/lib/wirefan \
    ghcr.io/ethany33/wirefan:latest
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
# unit file already points at /usr/local/bin/wirefan, no override needed
```

Either way, finish with:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now wirefan
sudo systemctl status wirefan
```

You should see `active (running)`. If not, jump to **Logs** in the operational
runbook section.

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
# expected: HTTP/1.1 200 OK + JSON body {"status":"ok",...}

curl -i https://wirefan.ethanyucetepe.dev/
# expected: HTTP/1.1 200 OK + the demo HTML page
```

End-to-end pubsub check:

1. Open `https://wirefan.ethanyucetepe.dev/` in two browser tabs.
2. In each tab, the demo client subscribes to a channel called `test`.
3. Publish a message in tab A -> tab B receives it within ~50 ms.

If TLS isn't working yet but `:80` is, you'll get a 308 redirect to https
(Caddy default). If you get connection-refused, ingress (step 7) is still
blocking.

---

## 9. Capture an admin Bearer + mint API keys

The admin Bearer is logged at process startup. With Docker (Option A):

```bash
sudo docker logs wirefan 2>&1 | grep -i "admin token"
```

With the native binary (Option B):

```bash
sudo journalctl -u wirefan | grep -i "admin token"
```

You should see a line with a token like
`admin token: wfa_live_...` — copy it.

> **Reminder:** until the env-var wiring task lands, this token is regenerated
> on every restart. If you set `WIREFAN_ADMIN_TOKEN=` in `/etc/wirefan/env`,
> grep won't find a generated one — you'll just use the value you set.

Mint a production API key for an app server to use:

```bash
curl -X POST https://wirefan.ethanyucetepe.dev/v1/keys \
    -H "Authorization: Bearer <admin-token-here>" \
    -H "Content-Type: application/json" \
    -d '{"name":"production-app"}'
```

Response includes a `secret` shown **once**. Save it in your app server's
config — you cannot recover it later.

---

## 10. Monitoring

The wirefan process exposes Prometheus metrics on the same port as the API:

```bash
# from inside the host
curl http://localhost:8080/metrics | head
```

You have two reasonable options for external scraping:

1. **Add a second Caddy site** for `metrics.wirefan.ethanyucetepe.dev`,
   protected by basic-auth or IP allowlist, that proxies to `:8080/metrics`.
   Quick, but exposes the metrics surface to the internet.
2. **Tailscale tunnel.** Install Tailscale on the Oracle host, scrape from a
   Prometheus running on your home network. No public exposure, no auth to
   manage. Recommended.

Either way, add scrape config to your Prometheus pointing at the metrics
endpoint. The wirefan histograms are documented in `docs/DESIGN.md`.

---

## 11. Run benchmarks against the production host

This is the Task 25 follow-up: capture real numbers from the deployed host.

On the Oracle instance (or any box that can reach it):

```bash
# Build the loadtest binary on the host so it's native arm64
cd /tmp/wirefan-src
go build -o /tmp/loadtest ./cmd/loadtest

# Mint a production-test key and export it
export WIREFAN_KEY=<key-id-from-step-9>
export WIREFAN_HOST=wirefan.ethanyucetepe.dev

bash scripts/bench.sh
```

The script writes pprof PNGs to `deploy/profiles/` and a markdown summary
fragment. Update `docs/BENCHMARKS.md` with the numbers and commit.

---

## 12. OG card real numbers + GitHub social preview

After step 11 you have real p99 / fanout-rate numbers. Update
`docs/og-card.svg` with them, rasterize:

```bash
# from a machine with rsvg-convert or Inkscape
rsvg-convert -w 1280 -h 640 docs/og-card.svg -o docs/og-card.png
```

Then go to GitHub repo -> **Settings** -> **General** -> **Social preview** ->
**Upload an image** -> pick `docs/og-card.png`. This is Task 34's deliverable.

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
   make build
   ./bin/wirefan
   ```
   (Or use the same systemd unit if you want it managed.)
7. **Run the tunnel.**
   ```bash
   cloudflared tunnel run wirefan
   ```
   Or install as a service:
   ```bash
   sudo cloudflared service install
   ```
8. **Smoke test** the same way as step 8.

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

The SQLite store at `/var/lib/wirefan/wirefan.db` holds API keys and any
persisted control-plane state. Back it up with rsync on a cron:

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

The `/v1/health` endpoint flips to `503 Service Unavailable` during graceful
shutdown so load balancers can drain:

```bash
curl -I http://localhost:8080/v1/health
# 200 OK in steady state; 503 during shutdown
```

### Process resource usage

```bash
sudo docker stats wirefan          # docker mode
top -p $(pgrep -f wirefan)         # native mode
```

If memory grows unboundedly, capture a heap profile:

```bash
curl http://localhost:8080/debug/pprof/heap > /tmp/heap.pprof
go tool pprof -png /tmp/heap.pprof > heap.png
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
sudo chown wirefan:wirefan /var/lib/wirefan/wirefan.db
sudo systemctl start wirefan
```

For full host loss: re-run steps 1-9 against a fresh A1 instance. The SQLite
backup contains the keys; everything else is rebuilt from `deploy/`.

---

## Cross-references

- `deploy/Dockerfile` — multi-stage build, distroless runtime, cgo for sqlite.
- `deploy/Caddyfile` — reverse-proxy + auto-TLS config.
- `deploy/wirefan.service` — systemd unit (binary-on-disk by default).
- `deploy/.env.example` — template for `/etc/wirefan/env`.
- `deploy/README.md` — short orientation to the deploy/ directory.
- `docs/DESIGN.md` — runtime architecture, fanout, backpressure.
- `docs/PROTOCOL.md` — wire protocol clients implement.
- `docs/BENCHMARKS.md` — numbers from the bench in step 11.
