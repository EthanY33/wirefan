# NOTE for Windows dev: GNU make is not on PATH in Git Bash. Every target
# here is a thin wrapper; the direct commands are listed per target so you
# can paste them into Git Bash without make.

.PHONY: build test test-race lint clean loadtest bench bench-image docs-sync

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

docs-sync:
	@echo "Manual reminder: keep ARCHITECTURE.md / DESIGN.md / PROTOCOL.md in sync."
	@echo "If you added or removed a package, update the repo map in ARCHITECTURE.md."
	@echo "If you changed wire-level behavior, update PROTOCOL.md."
	@echo "If you changed an architectural decision, update DESIGN.md."
