#!/usr/bin/env bash
# wirefan benchmark runner: matrix over {fanout} x {registry} in Docker.
#
# Each cell boots the server in its own container pinned to 1 CPU and 6 GB
# memory (docker run --cpus=1 --memory=6g), self-mints a pool of API keys
# against that cell's own admin endpoint, then drives it with cmd/loadtest
# from the host. Raw output lands in results/, one file per repetition.
#
# Key pool: the server rate-limits publishes per API key (100 msg/s, burst
# 200, see internal/ratelimit wiring in cmd/wirefan). The pool is sized at
# one key per KEYS_PER_CONNS(=10) connections so each key's aggregate
# publish rate (~5 publishers x RATE msg/s = 50 msg/s at defaults) stays
# well under the per-key limit. Sharing one key across all connections
# would measure the token bucket, not the fan-out engine.
#
# FAIL-HARD POLICY: any server death, health-check timeout, mint failure,
# loadtest nonzero exit (dial failures, server rejections, zero throughput)
# aborts the whole run with a nonzero exit. A zero-throughput cell is an
# error, never a published result.
#
# Usage:
#   go build -o bin/loadtest.exe ./cmd/loadtest        # host load generator
#   docker build -f deploy/Dockerfile -t wirefan:bench .
#   bash scripts/bench.sh
#
# Tunables (env): CONNS, CHANNELS, RATE, DURATION, RAMPUP, REPS, CELLS,
# PROFILE_CELL (cell label to pprof, e.g. "sharded-sharded"; empty = none).

set -euo pipefail
cd "$(dirname "$0")/.."

DURATION="${DURATION:-30s}"
RAMPUP="${RAMPUP:-5s}"
CONNS="${CONNS:-1000}"
CHANNELS="${CHANNELS:-100}"
RATE="${RATE:-10}"
REPS="${REPS:-3}"
KEYS_PER_CONNS="${KEYS_PER_CONNS:-10}"
IMAGE="${IMAGE:-wirefan:bench}"
if [ -z "${LOADTEST:-}" ]; then
  if [ -x ./bin/loadtest.exe ]; then LOADTEST=./bin/loadtest.exe; else LOADTEST=./bin/loadtest; fi
fi
CPUS="${CPUS:-1}"
MEMORY="${MEMORY:-6g}"
IP_CAP="${IP_CAP:-20000}"
PROFILE_CELL="${PROFILE_CELL:-}"
PROFILE_SECONDS="${PROFILE_SECONDS:-15}"
# CELLS: space-separated "fanout/registry" pairs
CELLS="${CELLS:-per-conn/sync-map per-conn/sharded sharded/sync-map sharded/sharded}"

CONTAINER=wirefan-bench
PUB_PORT=8080
ADMIN_PORT=6060
ADMIN="http://127.0.0.1:${ADMIN_PORT}"
PUBLIC="http://127.0.0.1:${PUB_PORT}"

mkdir -p results docs/profiles

[ -x "$LOADTEST" ] || { echo "FATAL: $LOADTEST missing (go build -o bin/loadtest.exe ./cmd/loadtest)" >&2; exit 1; }
docker image inspect "$IMAGE" >/dev/null 2>&1 || { echo "FATAL: image $IMAGE missing (docker build -f deploy/Dockerfile -t $IMAGE .)" >&2; exit 1; }

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

fail() { echo "FATAL: $*" >&2; docker logs "$CONTAINER" >&2 2>&1 || true; exit 1; }

# scrape_published <outfile-suffix-msg>: echo current counter value
scrape_published() {
  curl -fsS "${ADMIN}/metrics" | awk '/^wirefan_messages_published_total/ {print $2}'
}

run_cell() {
  local fanout="$1" registry="$2" rep="$3"
  local label="${fanout}-${registry}"
  local out="results/${label}-c${CONNS}-rep${rep}.txt"
  local token
  token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')

  echo "=== cell fanout=${fanout} registry=${registry} conns=${CONNS} rep=${rep} ==="
  cleanup

  local docker_cmd=(docker run -d --name "$CONTAINER" \
    --cpus="$CPUS" --memory="$MEMORY" \
    -e WIREFAN_ADMIN_TOKEN="$token" \
    -e WIREFAN_IP_CAP="$IP_CAP" \
    -p "${PUB_PORT}:8080" -p "127.0.0.1:${ADMIN_PORT}:6060" \
    "$IMAGE" \
    --listen=:8080 --admin-addr=0.0.0.0:6060 \
    --dev --allowed-origins='*' \
    --store=memory --registry="$registry" --fanout="$fanout")

  {
    echo "# wirefan bench raw output"
    echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "# cell: fanout=${fanout} registry=${registry} rep=${rep}"
    echo "# image: ${IMAGE}"
    echo "# docker invocation: ${docker_cmd[*]}"
    echo "# loadtest: ${LOADTEST} --addr=ws://127.0.0.1:${PUB_PORT} --conns=${CONNS} --channels=${CHANNELS} --rate=${RATE} --dur=${DURATION} --rampup=${RAMPUP} --keys=<pool of ${NKEYS} ids>"
    echo "# key pool: ${NKEYS} keys (1 per ${KEYS_PER_CONNS} conns); WIREFAN_IP_CAP=${IP_CAP}"
  } > "$out"

  "${docker_cmd[@]}" >/dev/null || fail "docker run failed for ${label}"

  # Wait for health (server must come up within ~15s)
  local ok=""
  for _ in $(seq 1 30); do
    if curl -fsS "${PUBLIC}/v1/health" >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  [ -n "$ok" ] || fail "server never became healthy for ${label}"

  # Self-mint the key pool against this cell's admin endpoint.
  local keys="" id i
  for i in $(seq 1 "$NKEYS"); do
    id=$(curl -fsS -X POST -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
      -d "{\"name\":\"bench-$i\"}" "${ADMIN}/v1/keys" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    [ -n "$id" ] || fail "key mint $i/${NKEYS} failed for ${label}"
    keys="$keys,$id"
  done
  keys="${keys#,}"

  local before after
  before=$(scrape_published) || fail "metrics scrape (before) failed for ${label}"

  # Optional pprof capture concurrent with the load (headline cell only).
  local pprof_pid=""
  if [ -n "$PROFILE_CELL" ] && [ "$label" = "$PROFILE_CELL" ] && [ "$rep" = "1" ]; then
    ( sleep 8; curl -fsS "${ADMIN}/debug/pprof/profile?seconds=${PROFILE_SECONDS}" -o "results/${label}-c${CONNS}-cpu.pb.gz" \
        && curl -fsS "${ADMIN}/debug/pprof/heap" -o "results/${label}-c${CONNS}-heap.pb.gz" ) &
    pprof_pid=$!
  fi

  # Drive the load. Loadtest exits nonzero on dial failures, server
  # rejections, or zero throughput; set -e makes that abort the matrix.
  "$LOADTEST" \
    --addr="ws://127.0.0.1:${PUB_PORT}" \
    --keys="$keys" \
    --conns="$CONNS" \
    --channels="$CHANNELS" \
    --rate="$RATE" \
    --dur="$DURATION" \
    --rampup="$RAMPUP" \
    2>&1 | tee -a "$out"
  local lt_status=${PIPESTATUS[0]}
  [ "$lt_status" = "0" ] || fail "loadtest exited ${lt_status} for ${label} rep ${rep} (see $out)"

  [ -z "$pprof_pid" ] || wait "$pprof_pid" || fail "pprof capture failed for ${label}"

  # Server container must still be alive (it dying mid-run = invalid cell).
  [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER")" = "true" ] || fail "server died during ${label} rep ${rep}"

  # Cross-check: server-side published counter vs client-side sent count.
  after=$(scrape_published) || fail "metrics scrape (after) failed for ${label}"

  # Server-side broadcast latency histogram (authoritative: the container's
  # Linux clock is nanosecond-resolution, unlike the Windows host wall clock
  # that quantizes client-observed latencies at ~0.5ms).
  curl -fsS "${ADMIN}/metrics" | grep '^wirefan_broadcast_latency_seconds' >> "$out" \
    || fail "latency histogram scrape failed for ${label}"
  awk '/^wirefan_broadcast_latency_seconds_sum/{s=$2} /^wirefan_broadcast_latency_seconds_count/{c=$2} END{if(c>0) printf "SERVER_LATENCY mean_us=%.2f count=%d\n", s/c*1e6, c}' "$out" | tee -a "$out"
  local delta sent
  delta=$(awk -v a="$after" -v b="$before" 'BEGIN{printf "%d", a-b}')
  sent=$(awk '/^SUMMARY /{for(i=1;i<=NF;i++) if($i ~ /^sent=/){split($i,kv,"="); print kv[2]}}' "$out")
  {
    echo "CROSSCHECK wirefan_messages_published_total_delta=${delta} client_sent=${sent}"
  } | tee -a "$out"
  if [ "$delta" != "$sent" ]; then
    echo "WARNING: server accepted ${delta} but client sent ${sent} for ${label} rep ${rep}" | tee -a "$out"
  fi
  [ "$delta" -gt 0 ] || fail "zero server-side throughput for ${label} rep ${rep}"

  cleanup
}

NKEYS=$(( (CONNS + KEYS_PER_CONNS - 1) / KEYS_PER_CONNS ))

for cell in $CELLS; do
  fanout="${cell%%/*}"
  registry="${cell##*/}"
  for rep in $(seq 1 "$REPS"); do
    run_cell "$fanout" "$registry" "$rep"
  done
done

echo
echo "All cells completed cleanly. Raw output in ./results/."
echo "Render pprof captures (if any): go tool pprof -top -text results/<cell>-cpu.pb.gz > docs/profiles/<cell>-cpu.txt"
