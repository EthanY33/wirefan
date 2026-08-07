#!/usr/bin/env bash
# deploy/provision.sh -- take a fresh Ubuntu 24.04 box to a running,
# TLS-terminated wirefan.
#
# Usage (as root, from a checkout or a copied deploy/ directory):
#
#   sudo ./provision.sh --domain wirefan.example.com [--binary /path/to/wirefan]
#
# What it does, in order:
#   1. sanity checks (root, Ubuntu, sibling template files present)
#   2. creates the `wirefan` system user (no shell, no home)
#   3. creates /var/lib/wirefan (state dir: admin.token + SQLite db),
#      owned wirefan:wirefan, mode 0700
#   4. installs /etc/wirefan/env from .env.example (kept if already present)
#      and pins WIREFAN_TRUSTED_PROXIES=127.0.0.1 for the local-Caddy topology
#   5. installs Caddy from the official apt repo
#   6. installs the Caddyfile and the systemd unit with your domain
#      substituted; a previous version that differs is saved to <file>.bak
#      before being replaced
#   7. installs the wirefan binary if --binary was given
#   8. enables + (re)starts wirefan and reloads Caddy (skipped with a loud
#      notice when systemd is not running, e.g. inside a container); re-runs
#      restart wirefan so a changed unit or binary actually takes effect
#
# Idempotent: safe to re-run. Existing /etc/wirefan/env is never overwritten.
# Derived files (unit, Caddyfile) are rewritten each run and are owned by
# this script: hand edits to them survive only in the .bak it leaves behind.
# If this box hosts other Caddy sites, merge them back from the .bak.
#
# Secrets: this script generates and prints none. The admin token is created
# by wirefan itself on first boot at /var/lib/wirefan/admin.token (mode 0600)
# and is never logged. Read it with: sudo cat /var/lib/wirefan/admin.token

set -euo pipefail

fail() {
    echo "provision.sh: FATAL: $*" >&2
    exit 1
}

trap 'echo "provision.sh: FATAL: aborted at line $LINENO (command failed). Nothing after this line ran." >&2' ERR

# --- argument parsing -------------------------------------------------------

DOMAIN=""
BINARY=""

while [ $# -gt 0 ]; do
    case "$1" in
        --domain)
            [ $# -ge 2 ] || fail "--domain requires a value"
            DOMAIN="$2"; shift 2 ;;
        --binary)
            [ $# -ge 2 ] || fail "--binary requires a value"
            BINARY="$2"; shift 2 ;;
        -h|--help)
            sed -n '2,25p' "$0"; exit 0 ;;
        *)
            fail "unknown argument: $1 (expected --domain <host> [--binary <path>])" ;;
    esac
done

[ -n "$DOMAIN" ] || fail "--domain is required, e.g. --domain wirefan.example.com"
case "$DOMAIN" in
    *[!A-Za-z0-9.-]*|"") fail "--domain '$DOMAIN' contains characters that are not [A-Za-z0-9.-]" ;;
esac
if [ -n "$BINARY" ] && [ ! -f "$BINARY" ]; then
    fail "--binary '$BINARY' does not exist"
fi

# --- sanity checks ----------------------------------------------------------

[ "$(id -u)" -eq 0 ] || fail "must run as root (sudo ./provision.sh ...)"

if [ -r /etc/os-release ]; then
    # shellcheck source=/dev/null
    . /etc/os-release
    if [ "${ID:-}" != "ubuntu" ]; then
        fail "this script targets Ubuntu (detected: ${ID:-unknown}). Aborting rather than guessing."
    fi
    case "${VERSION_ID:-}" in
        24.*) : ;;
        *) echo "provision.sh: WARNING: tested on Ubuntu 24.04, detected ${VERSION_ID:-unknown}. Continuing." >&2 ;;
    esac
else
    fail "/etc/os-release not readable; cannot confirm this is Ubuntu"
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
for f in wirefan.service Caddyfile .env.example; do
    [ -f "$SCRIPT_DIR/$f" ] || fail "template '$f' not found next to this script ($SCRIPT_DIR). Run from the repo's deploy/ directory or copy the whole directory."
done

HAVE_SYSTEMD=1
if [ ! -d /run/systemd/system ]; then
    HAVE_SYSTEMD=0
    echo "provision.sh: NOTICE: systemd is not running (container or chroot?)." >&2
    echo "provision.sh: NOTICE: files will be installed, but service enable/start is SKIPPED." >&2
fi

# --- 2. service user --------------------------------------------------------

if id -u wirefan >/dev/null 2>&1; then
    echo "provision.sh: user 'wirefan' already exists, keeping it"
else
    useradd --system --no-create-home --home-dir /var/lib/wirefan \
        --shell /usr/sbin/nologin wirefan
    echo "provision.sh: created system user 'wirefan'"
fi

# --- 3. state directory -----------------------------------------------------

install -d -o wirefan -g wirefan -m 0700 /var/lib/wirefan
echo "provision.sh: state dir /var/lib/wirefan ready (wirefan:wirefan, 0700)"

# --- 4. environment file ----------------------------------------------------

install -d -m 0755 /etc/wirefan
if [ -f /etc/wirefan/env ]; then
    echo "provision.sh: /etc/wirefan/env already exists, NOT overwriting"
else
    install -m 0600 -o root -g root "$SCRIPT_DIR/.env.example" /etc/wirefan/env
    echo "provision.sh: installed /etc/wirefan/env from .env.example (mode 0600)"
fi
# Caddy proxies from loopback in this topology; without this the per-IP
# connection cap collapses into a global cap (see .env.example).
if grep -Eq '^WIREFAN_TRUSTED_PROXIES=' /etc/wirefan/env; then
    echo "provision.sh: WIREFAN_TRUSTED_PROXIES already set in /etc/wirefan/env, keeping it"
else
    printf '\n# Set by provision.sh: Caddy proxies from loopback on this host.\nWIREFAN_TRUSTED_PROXIES=127.0.0.1\n' >> /etc/wirefan/env
    echo "provision.sh: set WIREFAN_TRUSTED_PROXIES=127.0.0.1 in /etc/wirefan/env"
fi

# --- 5. Caddy ---------------------------------------------------------------

if command -v caddy >/dev/null 2>&1; then
    echo "provision.sh: caddy already installed ($(caddy version | head -n1)), skipping install"
else
    echo "provision.sh: installing Caddy from the official apt repo"
    apt-get update -qq
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gnupg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
        | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
        > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq
    apt-get install -y -qq caddy
fi

# --- 6. config files (derived, rewritten every run) -------------------------

# Render to a temp file first; if the destination exists with different
# content (a hand edit, or another Caddy site on this box), keep a .bak so
# the overwrite is recoverable.
install_derived() {
    # $1 = template, $2 = destination
    local tmp
    tmp="$(mktemp)"
    sed "s/wirefan\.example\.com/$DOMAIN/g" "$1" > "$tmp"
    if [ -f "$2" ] && ! cmp -s "$tmp" "$2"; then
        cp -f "$2" "$2.bak"
        echo "provision.sh: NOTICE: $2 differed; previous version saved to $2.bak" >&2
    fi
    install -m 0644 -o root -g root "$tmp" "$2"
    rm -f "$tmp"
}

install_derived "$SCRIPT_DIR/Caddyfile" /etc/caddy/Caddyfile
echo "provision.sh: wrote /etc/caddy/Caddyfile for $DOMAIN"

install_derived "$SCRIPT_DIR/wirefan.service" /etc/systemd/system/wirefan.service
echo "provision.sh: wrote /etc/systemd/system/wirefan.service (allowed origin: https://$DOMAIN)"

# --- 7. binary --------------------------------------------------------------

if [ -n "$BINARY" ]; then
    install -m 0755 -o root -g root "$BINARY" /usr/local/bin/wirefan
    echo "provision.sh: installed $BINARY -> /usr/local/bin/wirefan"
elif [ -x /usr/local/bin/wirefan ]; then
    echo "provision.sh: /usr/local/bin/wirefan already present, keeping it"
else
    echo "provision.sh: NOTICE: no binary at /usr/local/bin/wirefan and no --binary given." >&2
    echo "provision.sh: NOTICE: install one with deploy/deploy.sh before the service can start." >&2
fi

# --- 8. enable + start ------------------------------------------------------

if [ "$HAVE_SYSTEMD" -eq 1 ]; then
    systemctl daemon-reload
    if [ -x /usr/local/bin/wirefan ]; then
        systemctl enable wirefan
        # restart, not `enable --now`: on a re-run the service is already
        # active and `--now` would be a no-op, silently leaving a changed
        # unit or binary without effect.
        systemctl restart wirefan
        systemctl reload-or-restart caddy
        echo "provision.sh: wirefan enabled and (re)started; caddy reloaded"
        echo "provision.sh: verify with: curl -fsS https://$DOMAIN/v1/health   (expect: ok)"
        echo "provision.sh: admin token (first boot writes it): sudo cat /var/lib/wirefan/admin.token"
    else
        systemctl enable wirefan
        echo "provision.sh: wirefan enabled but NOT started (no binary yet); run deploy/deploy.sh"
    fi
else
    echo "provision.sh: SKIPPED systemd enable/start (no systemd in this environment)."
    echo "provision.sh: on a real host, finish with: systemctl daemon-reload && systemctl enable --now wirefan"
fi

echo "provision.sh: DONE"
