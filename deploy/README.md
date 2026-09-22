# wirefan deployment artifacts

This directory is the source of truth for deployment configuration. The
target is deliberately the lowest common denominator: a fresh Ubuntu 24.04
LTS VPS with SSH and a public IP, amd64 or arm64. That covers Hetzner,
DigitalOcean, Oracle Cloud, Lightsail, Vultr, and anything comparable.
Nothing here depends on a specific provider's API or CLI.

The full runbook is `docs/DEPLOY.md`. Short version:

```bash
# on the server, from a copy of this directory, with a release binary in ~
# (named wirefan_<version>_linux_<arch>; see docs/DEPLOY.md step 4)
sudo ./provision.sh --domain wirefan.example.com --binary ~/wirefan_v1.0.0_linux_amd64
```

## Files

| File | Purpose |
|---|---|
| `provision.sh` | Idempotent fresh-box setup: service user, state dir (0700), env file, Caddy install, unit + Caddyfile with your domain substituted, enable + start |
| `deploy.sh` | Upgrade: SHA-256-verify a new binary, stop, swap, start, health-check `/v1/health`, auto-rollback to the kept previous binary on failure |
| `wirefan.service` | systemd unit with hardening flags; runs `/usr/local/bin/wirefan` as the `wirefan` user |
| `Caddyfile` | Reverse proxy to `127.0.0.1:8080` with auto-Let's Encrypt TLS and WS-friendly flushing. Leaves `X-Forwarded-For` to Caddy's defaults: a client-sent value is discarded and replaced with the client address, which wirefan reads because `WIREFAN_TRUSTED_PROXIES=127.0.0.1`. Behind Cloudflare, see `docs/DEPLOY.md` Appendix C |
| `.env.example` | Template for `/etc/wirefan/env` (sourced by systemd); documents `WIREFAN_TRUSTED_PROXIES` and friends |
| `Dockerfile` | Optional container path: multi-stage build, Debian Bookworm builder + distroless runtime, cgo-enabled for sqlite3. Also used by the benchmark harness (`make bench-image`) |

Release binaries for linux/amd64 and linux/arm64 are built by
`.github/workflows/release.yml` on tag push (native runners per arch because
the SQLite driver needs cgo) as `wirefan_<tag>_linux_amd64` and
`wirefan_<tag>_linux_arm64`, with a `SHA256SUMS` file attached to the
GitHub release. `make release-local` produces the same file names in
`dist/` via Docker, with `<tag>` taken from `git describe --tags --always
--dirty` (override with `VERSION=`).

## Verifying the Docker image locally

`--allowed-origins` is required, so a bare `docker run` exits immediately
with a usage error. The admin listener must bind `0.0.0.0` inside the
container to be reachable through the port mapping.

```bash
docker build -t wirefan:latest -f deploy/Dockerfile .

docker run --rm --name wirefan \
    -p 8080:8080 -p 127.0.0.1:6060:6060 \
    wirefan:latest \
    --listen=:8080 --admin-addr=0.0.0.0:6060 --dev --allowed-origins='*'

# Then in another shell:
curl -i http://localhost:8080/v1/health    # 200, body "ok"

# Mint a key (the admin token is persisted inside the container, never
# printed; distroless has no shell, so read it with docker cp):
docker cp wirefan:/var/lib/wirefan/admin.token ./admin.token
curl -s -X POST http://127.0.0.1:6060/v1/keys \
    -H "Authorization: Bearer $(cat admin.token)" \
    -d '{"name":"smoke"}'
```
