#!/usr/bin/env bash
# deploy/deploy.sh -- checksum-verified binary swap with health check and
# automatic rollback.
#
# Usage (as root, on the server):
#
#   sudo ./deploy.sh <new-binary> <sha256>
#
#   <new-binary>  path to the freshly downloaded/copied wirefan binary
#   <sha256>      either a 64-hex-char digest, or a checksum file in
#                 `sha256sum` format (e.g. the SHA256SUMS from a release)
#                 that contains an entry matching the binary's basename
#
# What it does, in order:
#   1. verifies the SHA-256 of <new-binary> against <sha256>; refuses to
#      continue on mismatch
#   2. stages a private copy of the verified binary, so the swap below
#      operates on a snapshot; this makes it safe to pass
#      /usr/local/bin/wirefan.prev itself as <new-binary>, which is
#      exactly what the manual-rollback command in docs/DEPLOY.md does
#   3. stops wirefan
#   4. snapshots the stopped database: /var/lib/wirefan/wirefan.db and any
#      -wal/-shm sidecars are copied (cp -p) to <file>.prev, replacing the
#      previous run's snapshot
#   5. saves the currently installed binary to /usr/local/bin/wirefan.prev
#      (exactly one previous version is kept, for rollback)
#   6. installs the staged binary at /usr/local/bin/wirefan
#   7. starts wirefan (a failed start falls through to the health check
#      rather than aborting) and polls http://127.0.0.1:8080/v1/health
#      for up to 30s expecting HTTP 200
#   8. on health-check failure: stops wirefan, restores the database
#      snapshot, puts wirefan.prev back, restarts, re-checks, and exits
#      non-zero either way
#
# The database snapshot exists because a new version may migrate the schema
# on its first start, and an older binary refuses to open a database whose
# schema is newer than it knows. Rolling back the binary alone would then
# leave the service down. After a successful upgrade the snapshot stays in
# place for a manual rollback (docs/DEPLOY.md, step 7); the next run of this
# script replaces it. The admin token is never touched.

set -euo pipefail

BIN_PATH=/usr/local/bin/wirefan
PREV_PATH=/usr/local/bin/wirefan.prev
HEALTH_URL=http://127.0.0.1:8080/v1/health
HEALTH_TIMEOUT=30
# Matches --db-path in deploy/wirefan.service; change both together.
DB_PATH=/var/lib/wirefan/wirefan.db
# The database plus its WAL sidecars. They are snapshotted and restored as
# one set: a -wal left over from a different moment than the main file
# would be replayed on top of it.
DB_FILES=("$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm")

fail() {
    echo "deploy.sh: FATAL: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || fail "must run as root (sudo ./deploy.sh ...)"
[ $# -eq 2 ] || fail "usage: deploy.sh <new-binary> <sha256-digest-or-checksum-file>"

NEW_BINARY="$1"
CHECKSUM_ARG="$2"

[ -f "$NEW_BINARY" ] || fail "new binary '$NEW_BINARY' does not exist"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum not found"
command -v curl >/dev/null 2>&1 || fail "curl not found"

# --- 1. checksum ------------------------------------------------------------

ACTUAL="$(sha256sum "$NEW_BINARY" | awk '{print $1}')"

if [ -f "$CHECKSUM_ARG" ]; then
    BASE="$(basename "$NEW_BINARY")"
    EXPECTED="$(awk -v f="$BASE" '$2 == f || $2 == "*"f {print $1}' "$CHECKSUM_ARG" | head -n1)"
    [ -n "$EXPECTED" ] || fail "checksum file '$CHECKSUM_ARG' has no entry for '$BASE'"
else
    EXPECTED="$(printf '%s' "$CHECKSUM_ARG" | tr '[:upper:]' '[:lower:]')"
    case "$EXPECTED" in
        *[!0-9a-f]*|"") fail "'$CHECKSUM_ARG' is neither an existing file nor a hex digest" ;;
    esac
    [ "${#EXPECTED}" -eq 64 ] || fail "digest must be 64 hex chars, got ${#EXPECTED}"
fi

if [ "$ACTUAL" != "$EXPECTED" ]; then
    fail "SHA-256 MISMATCH for $NEW_BINARY
  expected: $EXPECTED
  actual:   $ACTUAL
Refusing to install."
fi
echo "deploy.sh: checksum OK ($ACTUAL)"

[ -d /run/systemd/system ] || fail "systemd is not running; this script manages the wirefan systemd service"

# --- 2. stage the verified binary -------------------------------------------
# Copy the verified binary aside BEFORE anything touches $PREV_PATH. Without
# this, running the documented manual rollback (which passes $PREV_PATH as
# <new-binary>) would first overwrite the rollback copy with the currently
# installed bad binary, then reinstall that same bad binary, while reporting
# success. Staging on the same filesystem as $BIN_PATH keeps the later
# install a local copy, not a cross-device move.
STAGED="$(mktemp /usr/local/bin/.wirefan.staged.XXXXXX)" || fail "mktemp failed"
trap 'rm -f "$STAGED"' EXIT
cp -f "$NEW_BINARY" "$STAGED"

# Wall-clock deadline: each probe can spend up to 2s in curl plus the 1s
# sleep, so counting iterations would overshoot the advertised timeout.
health_check() {
    local deadline=$((SECONDS + HEALTH_TIMEOUT))
    while [ "$SECONDS" -lt "$deadline" ]; do
        if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# Both helpers run only while wirefan is stopped, so the files are quiescent.
# Each step checks its own status because callers use them in `if !`/`||`
# context, where set -e does not apply inside the function.
#
# snapshot_db copies every DB file that exists to <file>.prev and removes
# any .prev whose source is absent, so the .prev set always describes one
# moment rather than mixing in sidecars from an older run.
snapshot_db() {
    local f
    for f in "${DB_FILES[@]}"; do
        rm -f "$f.prev" || return 1
        if [ -f "$f" ]; then
            cp -p "$f" "$f.prev" || return 1
        fi
    done
}

# restore_db makes the DB files match the snapshot exactly: files with a
# .prev are copied back, files without one are removed (a -wal the new
# binary left behind must not be replayed onto the restored database).
restore_db() {
    local f
    for f in "${DB_FILES[@]}"; do
        if [ -f "$f.prev" ]; then
            cp -p "$f.prev" "$f" || return 1
        else
            rm -f "$f" || return 1
        fi
    done
}

# --- 3-7. stop, snapshot, swap, start ---------------------------------------

echo "deploy.sh: stopping wirefan"
systemctl stop wirefan

if ! snapshot_db; then
    # Nothing has been swapped yet, so the current binary can come back up.
    systemctl start wirefan || true
    fail "could not snapshot $DB_PATH to $DB_PATH.prev (disk full?); nothing was swapped and wirefan was restarted on the current binary"
fi
echo "deploy.sh: snapshotted $DB_PATH (and any -wal/-shm) to .prev"

HAD_PREVIOUS=0
if [ -f "$BIN_PATH" ]; then
    cp -f "$BIN_PATH" "$PREV_PATH"
    HAD_PREVIOUS=1
    echo "deploy.sh: saved current binary to $PREV_PATH"
else
    echo "deploy.sh: NOTICE: no existing binary at $BIN_PATH (first deploy); rollback will not be possible"
fi

install -m 0755 -o root -g root "$STAGED" "$BIN_PATH"
echo "deploy.sh: installed new binary at $BIN_PATH"

echo "deploy.sh: starting wirefan"
# Not fatal on purpose: a start failure (e.g. exec format error from a
# wrong-arch binary) must fall through to the health check so the rollback
# block below runs, instead of set -e exiting with the bad binary installed.
systemctl start wirefan \
    || echo "deploy.sh: WARNING: systemctl start failed; falling through to health check + rollback" >&2

# --- 7-8. health check, rollback on failure ---------------------------------

if health_check; then
    echo "deploy.sh: health check OK ($HEALTH_URL responded 200 'ok')"
    echo "deploy.sh: DONE. Previous binary kept at $PREV_PATH and pre-upgrade database at $DB_PATH.prev for manual rollback."
    exit 0
fi

echo "deploy.sh: HEALTH CHECK FAILED after ${HEALTH_TIMEOUT}s. Recent logs:" >&2
journalctl -u wirefan -n 20 --no-pager >&2 || true

if [ "$HAD_PREVIOUS" -eq 1 ]; then
    echo "deploy.sh: ROLLING BACK to $PREV_PATH and the pre-upgrade database" >&2
    systemctl stop wirefan || true
    # The new binary may already have migrated the schema, which the
    # previous binary would refuse to open.
    restore_db || fail "new binary failed its health check AND restoring the database from $DB_PATH.prev failed. Service is stopped; restore the *.prev files in $(dirname "$DB_PATH") by hand"
    install -m 0755 -o root -g root "$PREV_PATH" "$BIN_PATH"
    # || true so a failed start still reaches the explanatory fail below
    # instead of exiting silently with systemd's status code.
    systemctl start wirefan || true
    if health_check; then
        fail "new binary failed its health check; ROLLED BACK to previous binary and pre-upgrade database, which are healthy again"
    else
        fail "new binary failed AND rollback failed its health check. Service is down; investigate with: journalctl -u wirefan -n 100"
    fi
else
    fail "new binary failed its health check and there is no previous binary to roll back to. Service is down; investigate with: journalctl -u wirefan -n 100"
fi
