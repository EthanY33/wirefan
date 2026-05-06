# Multi-stage build for wirefan.
#
# Stage 1: Debian Bookworm builder with the Go toolchain + a C toolchain so
# mattn/go-sqlite3 (cgo-only) can compile. Building on Debian (glibc) and
# running on distroless/base-debian12 (also glibc) keeps the libc ABI matched.
#
# Stage 2: distroless/base-debian12 — minimal runtime that still ships glibc,
# so the cgo-linked binary loads cleanly. Runs as the built-in `nonroot` user.
FROM golang:1.25-bookworm AS builder

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

FROM gcr.io/distroless/base-debian12

COPY --from=builder /out/wirefan /wirefan
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/wirefan"]
