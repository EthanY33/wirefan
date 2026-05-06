.PHONY: build test test-race lint clean loadtest bench

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
	@echo "Run: ./bin/loadtest --key=<api-key-id> --conns=1000 --channels=100 --rate=10 --dur=30s"
