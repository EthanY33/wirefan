#!/usr/bin/env bash
# wirefan benchmark runner — matrix over fanout/registry strategies
#
# Pre-reqs: ./bin/wirefan and ./bin/loadtest exist (run `make build loadtest`)
#
# NOTE (as of this commit): --fanout and --registry flags are NOT YET wired into
# cmd/wirefan; the server defaults to per-conn fanout + sync-map registry. This
# script's matrix is structured for forward-compat — uncomment the flag args
# once flag wiring lands. For now, every cell exercises the default config.

set -euo pipefail

mkdir -p results docs/profiles

DURATION="${DURATION:-30s}"
CONNS="${CONNS:-1000}"
CHANNELS="${CHANNELS:-100}"
RATE="${RATE:-10}"

if [ -z "${WIREFAN_KEY:-}" ]; then
  echo "Set WIREFAN_KEY to a valid API key id (create one via POST /v1/keys)." >&2
  exit 1
fi

run_cell() {
  local fanout="$1"
  local registry="$2"
  local label="${fanout}-${registry}"
  echo "=== fanout=${fanout} registry=${registry} ==="

  # Once flags are wired, replace this with:
  #   ./bin/wirefan --fanout="$fanout" --registry="$registry" --port=8080 &
  ./bin/wirefan &
  local pid=$!
  sleep 1

  ./bin/loadtest \
    --addr=ws://localhost:8080 \
    --key="$WIREFAN_KEY" \
    --conns="$CONNS" \
    --channels="$CHANNELS" \
    --rate="$RATE" \
    --dur="$DURATION" \
    | tee "results/${label}.txt"

  # Capture pprof while a fresh load run hammers the server. Requires graphviz `dot` on PATH.
  # Uncomment once load is steady:
  # go tool pprof -png "http://localhost:8080/debug/pprof/profile?seconds=15" > "docs/profiles/${label}-cpu.png"
  # go tool pprof -png "http://localhost:8080/debug/pprof/heap"             > "docs/profiles/${label}-heap.png"

  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

for fanout in per-conn sharded; do
  for registry in sync-map sharded; do
    run_cell "$fanout" "$registry"
  done
done

echo
echo "Results in ./results/, profiles in docs/profiles/."
echo "Update docs/BENCHMARKS.md tables with the captured numbers."
