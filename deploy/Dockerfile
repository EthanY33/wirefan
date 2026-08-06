# Multi-stage build for wirefan.
#
# Stage 1: Debian Bookworm builder with the Go toolchain + a C toolchain so
# mattn/go-sqlite3 (cgo-only) can compile. Building on Debian (glibc) and
# running on distroless/base-debian12 (also glibc) keeps the libc ABI matched.
#
# Stage 2: distroless/base-debian12 — minimal runtime that still ships glibc,
# so the cgo-linked binary loads cleanly. Runs as the built-in `nonroot` user
# (uid 65532), which cannot write /var. The state dir (admin token + SQLite
# db) therefore lives at /var/lib/wirefan, created below and owned by
# nonroot, and exported via WIREFAN_STATE_DIR.
FROM golang:1.26-bookworm AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO required for mattn/go-sqlite3. -s -w strips debug info to shrink the
# binary. Native arch by default — for cross-builds use `docker buildx build
# --platform linux/arm64` from an x86_64 dev box.
RUN CGO_ENABLED=1 go build \
    -ldflags="-s -w" \
    -o /out/wirefan \
    ./cmd/wirefan

# Empty state dir staged here so the runtime image gets it with nonroot
# ownership (distroless has no shell, so no mkdir/chown at runtime).
RUN mkdir -p /out/state

FROM gcr.io/distroless/base-debian12

COPY --from=builder /out/wirefan /wirefan
COPY --from=builder --chown=nonroot:nonroot /out/state /var/lib/wirefan

# admin.token and wirefan.db land here. Mount a volume over it to persist
# API keys and the admin token across container replacements.
ENV WIREFAN_STATE_DIR=/var/lib/wirefan
VOLUME ["/var/lib/wirefan"]

# 8080: public listener (WS, /v1/health, demo). 6060: admin listener
# (/v1/keys, /metrics, /debug/pprof) — publish it bound to loopback only,
# e.g. `-p 127.0.0.1:6060:6060`.
EXPOSE 8080 6060

USER nonroot:nonroot

# No default args: --allowed-origins is required and deployment-specific, so
# the container exits immediately with a usage error unless you pass flags.
# Working invocation (admin listener must bind 0.0.0.0 inside the container
# to be reachable through the port mapping):
#
#   docker run --rm \
#     -p 8080:8080 -p 127.0.0.1:6060:6060 \
#     -v wirefan-state:/var/lib/wirefan \
#     wirefan:dev \
#     --listen=:8080 --admin-addr=0.0.0.0:6060 \
#     --allowed-origins=https://your.host
#
# For a local smoke test, use `--dev --allowed-origins='*'` instead of a real
# origin. Full runbook: docs/DEPLOY.md.
ENTRYPOINT ["/wirefan"]
