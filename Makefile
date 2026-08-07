# NOTE for Windows dev: GNU make is not on PATH in Git Bash. Every target
# here is a thin wrapper; the direct commands are listed per target so you
# can paste them into Git Bash without make.

.PHONY: build test test-race lint clean loadtest bench bench-image docs-sync release-local

# Direct: go build -o bin/wirefan ./cmd/wirefan   (bin/wirefan.exe on Windows)
build:
	go build -o bin/wirefan ./cmd/wirefan

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/

# Direct: go build -o bin/loadtest ./cmd/loadtest (bin/loadtest.exe on Windows)
loadtest:
	go build -o bin/loadtest ./cmd/loadtest

# Server image used by the benchmark matrix (scripts/bench.sh boots it with
# --cpus/--memory limits). Direct: docker build -f deploy/Dockerfile -t wirefan:bench .
bench-image:
	docker build -f deploy/Dockerfile -t wirefan:bench .

# Full dockerized matrix. Requires Docker and the wirefan:bench image.
# Direct path without make (e.g. Git Bash on Windows):
#   go build -o bin/loadtest.exe ./cmd/loadtest
#   docker build -f deploy/Dockerfile -t wirefan:bench .
#   bash scripts/bench.sh
# Tunables (env): CONNS, CHANNELS, RATE, DURATION, RAMPUP, REPS, CELLS.
# See docs/BENCHMARKS.md for the published methodology.
bench: loadtest bench-image
	bash scripts/bench.sh

# Linux release binaries (amd64 + arm64) + SHA256SUMS into dist/, built
# inside golang:1.26-bookworm. CGO is required (mattn/go-sqlite3), so the
# arm64 build uses the gcc-aarch64-linux-gnu cross compiler. Bookworm's
# glibc (2.36) is older than Ubuntu 24.04's (2.39), so these binaries run
# on Ubuntu 24.04 targets. CI (.github/workflows/release.yml) builds the
# published release binaries natively on amd64/arm64 runners instead; this
# target is for local verification and ad-hoc deploys.
# Direct: see the docker run command below; works from Git Bash on Windows
# (MSYS_NO_PATHCONV=1 may be needed for the volume mount).
release-local:
	docker run --rm -v "$(CURDIR)":/src -w /src golang:1.26-bookworm bash -c '\
		set -euo pipefail; \
		apt-get update -qq && apt-get install -y -qq gcc gcc-aarch64-linux-gnu >/dev/null; \
		mkdir -p dist; \
		CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/wirefan_linux_amd64 ./cmd/wirefan; \
		CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc go build -trimpath -ldflags="-s -w" -o dist/wirefan_linux_arm64 ./cmd/wirefan; \
		cd dist && sha256sum wirefan_linux_amd64 wirefan_linux_arm64 > SHA256SUMS && cat SHA256SUMS'

docs-sync:
	@echo "Manual reminder: keep ARCHITECTURE.md / DESIGN.md / PROTOCOL.md in sync."
	@echo "If you added or removed a package, update the repo map in ARCHITECTURE.md."
	@echo "If you changed wire-level behavior, update PROTOCOL.md."
	@echo "If you changed an architectural decision, update DESIGN.md."
