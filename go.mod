module github.com/EthanY33/wirefan

// Consumers building this module (CI go-version, deploy/Dockerfile builder
// stage) need a Go 1.26 toolchain; keeping the go directive at the minor
// release avoids forcing every downstream past each patch bump.
go 1.26.0

toolchain go1.26.8

require (
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/oklog/ulid/v2 v2.1.2
)

require github.com/coder/websocket v1.8.15

require (
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.3
	golang.org/x/time v0.16.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
