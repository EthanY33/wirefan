.PHONY: build test test-race lint clean loadtest bench docs-sync

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

loadtest:
	go build -o bin/loadtest ./cmd/loadtest

bench: build loadtest
	@bash scripts/bench.sh

docs-sync:
	@echo "Manual reminder: keep ARCHITECTURE.md / DESIGN.md / PROTOCOL.md in sync."
	@echo "If you added or removed a package, update the repo map in ARCHITECTURE.md."
	@echo "If you changed wire-level behavior, update PROTOCOL.md."
	@echo "If you changed an architectural decision, update DESIGN.md."
