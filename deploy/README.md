# wirefan deployment artifacts

This directory is the source of truth for deployment configuration. Files here
are intended for the Oracle Cloud Always Free Ampere A1 instance, but most are
adaptable to any Linux ARM64 / amd64 host.

## Files

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage build, Debian Bookworm builder + distroless base runtime, cgo-enabled for sqlite3 |
| `Caddyfile` | Reverse proxy with auto-Let's Encrypt TLS, WS-friendly buffering, X-Forwarded-For passthrough |
| `wirefan.service` | systemd unit with hardening flags and persistent volume mount |
| `.env.example` | Template for `/etc/wirefan/env` (sourced by systemd) |

## Quick deploy outline (Oracle Ampere A1)

See `docs/DEPLOY.md` for the full runbook. Short version:

1. Provision Ampere A1 (1 OCPU / 6 GB) via Oracle console
2. Install Docker + Caddy
3. Build image: `docker build -t wirefan:latest -f deploy/Dockerfile .`
4. Push to ghcr.io: `docker tag wirefan:latest ghcr.io/ethany33/wirefan:latest && docker push ...`
5. On the host: `docker pull ...`, set up `/etc/wirefan/env`, create `wirefan` user + `/var/lib/wirefan` dir, install `wirefan.service`, `systemctl enable --now wirefan`
6. Set up Caddyfile, point DNS, verify TLS at `https://<your-domain>/v1/health`

## Cross-build for ARM64 from x86 dev box

```bash
# From repo root, if developing on x86_64:
docker buildx build --platform linux/arm64 -t wirefan:arm64 -f deploy/Dockerfile .
```

## Verifying the image locally

`--allowed-origins` is required, so a bare `docker run` exits immediately
with a usage error. The admin listener must bind `0.0.0.0` inside the
container to be reachable through the port mapping.

```bash
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
