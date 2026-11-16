# 01. Abuse resistance

Epic label: `epic:abuse-resistance`. Tracking issue: [#95](https://github.com/EthanY33/wirefan/issues/95). Milestone: v1.1. Estimate: 24 h.

## Goal

Make the public listener and the demo host resistant to the abuse that a
public, keyed demo attracts, without changing behavior for well-behaved
clients:

1. One source IP on a shared key cannot starve other IPs on that key.
2. `/v1/auth/sign` cannot be used as an unthrottled credential oracle, and
   only mints tokens a real subscribe could use.
3. The demo VM's SSH, fail2ban, connection limits and systemd sandbox are
   captured in the repository and reproducible.

## Current state

* **Shared per-key bucket.** `internal/conn/handler.go: handlePublish`
  (around lines 226-232) and `allowControl` (around lines 252-258) charge
  the per-conn bucket and then `c.rateLimit.Allow(c.apiKeyID)`. The per-key
  limiter is one `ratelimit.New(100, 200, time.Hour)` in
  `cmd/wirefan/main.go: run`, shared by every connection opened with that
  key. On the demo, every visitor uses the same public key id, so a single
  visitor publishing at the per-conn limit across a few sockets drains the
  key's 100/s budget and everyone else gets `RATE_LIMITED`.
* **Client IP is known but not passed down.** `internal/server/upgrade.go`
  computes `ipCapKey(clientIP(r, trustedProxies))` for the per-IP connection
  cap (around line 275), but `conn.Run` (`internal/conn/conn.go:169`) and
  `conn.Deps` carry no address.
* **Sign is unthrottled.** `internal/server/rest.go: sign` (lines 166-205)
  runs `store.LookupKey` and a constant-time secret compare per request on
  the public listener with no limiter. `internal/metrics/prom.go`
  `AuthFails` counts only failed subscribe tokens.
* **Sign does not check the socket.** `sign` checks only that `socket_id`
  parses as a ULID. `docs/DESIGN.md` section 5.3 (lines 599-607) states that
  it does not check the socket belongs to a connection opened with the
  calling key. `internal/hub/hub.go` tracks conns in a set keyed by the conn
  (`trackedConn`, lines 13-21) with no socket id accessor or index;
  `conn.Conn` holds `socketID` (`internal/conn/conn.go:57`).
* **Host hardening is not in the repo.** `deploy/demo/` holds only
  `egress-guard`, `egress-guard.service`, `egress-guard.timer` and
  `egress-shape.service`. `docs/DEPLOY.md` (around line 125) opens `22/tcp`
  with ufw. Nothing captures sshd settings, fail2ban or host-level
  connection limits.
* **systemd sandbox is partial.** `deploy/wirefan.service` lacks
  `CapabilityBoundingSet`, `RestrictAddressFamilies`, `SystemCallFilter`,
  `ProtectClock`, `ProtectKernelLogs`, `ProtectProc`, `ProcSubset`,
  `UMask`, `StateDirectory`, `TasksMax` and `LimitNOFILE`; the state
  directory is created by `deploy/provision.sh` and listed under
  `ReadWritePaths`.

## Proposed design

### Per-IP sub-limit inside a key

Add a second `ratelimit.Limiter` keyed by `apiKeyID + "|" + ipKey`, where
`ipKey` is the existing `ipCapKey` output (IPv4 address, or the IPv6 /64).
The order of checks for publish and control frames becomes:

```
per-conn bucket  ->  per-(key, IP) bucket  ->  shared per-key bucket
```

A frame refused by an earlier bucket spends nothing from later ones,
matching the rule in `docs/PROTOCOL.md` section 9. The sub-limit is off by
default (`WIREFAN_KEY_IP_RATE` and `WIREFAN_KEY_IP_BURST` unset), so 1.x
behavior is unchanged; the demo sets it. If the resource-limit flags from
the operability epic have landed, matching `--key-ip-rate` and
`--key-ip-burst` flags are added with the env vars as fallbacks.

`UpgradeHandler.ServeHTTP` already has the IP; it is passed into `conn.Run`
through a new `Deps`-adjacent argument (or a field on `Conn` set by the
caller). The same plumbing is reused by the admin kick endpoint in the
key-management epic.

Error code: the over-limit IP receives `RATE_LIMITED`, the same code as the
shared bucket, with a distinct message (`"too many requests from this
address for this API key"`). No new code is needed because the client's
correct reaction (back off) is identical. The refusal is counted in
`wirefan_rate_limited_total{scope="key_ip"}`; that metric is owned by the
operability epic and lands first.

### Sign endpoint throttling

Reuse `internal/ratelimit.Limiter`:

* **Failure limiter** keyed by client IP (`clientIP` and `trustedProxies`
  moved from `upgrade.go` into a small shared helper in `internal/server`).
  Default 10 per minute, burst 20. Charged only when credentials fail, so a
  correct app server never touches it. Applied to `requireAdmin` failures
  as well.
* **Success limiter** keyed by key id, configured by `WIREFAN_SIGN_RATE`
  (and `WIREFAN_SIGN_BURST`), unset by default, meaning unlimited.

A throttled request gets `429 Too Many Requests` with `Retry-After`. Counter
`wirefan_sign_requests_total{result="ok|bad_credentials|bad_request|throttled"}`
is added by this issue (the operability metrics issue does not add it).

### Sign socket binding

Add a socket-id index to the Hub:

```go
// in internal/hub/hub.go
type trackedConn interface {
    APIKeyID() string
    SocketID() string
    CloseFrame(code websocket.StatusCode, reason string)
    CloseNow()
}

func (h *Hub) LookupSocket(socketID string) (keyID string, ok bool)
```

The index is a `map[string]trackedConn` maintained in `Add` and `Remove`
under the existing lock. `sign` then applies two checks after credential
verification:

1. **Unknown or closed socket: always refused** with the existing `400`
   and body `bad request: socket_id is not a live socket`. Socket ids are
   ULIDs minted once per connection, `Hub.Add` runs before the `connected`
   frame announces the id, and a token is usable only by that socket, so a
   token for a socket that is not live could never be used. This extends
   the existing "only mint tokens that a real subscribe could use" rule in
   `sign` and uses a status code the endpoint already returns.
2. **Socket opened with a different key: refused when
   `--sign-same-key-only` is set** (default off in 1.x), with `403`. Some
   deployments may let browsers connect with one public key while an app
   server signs with another key, which 1.0 permits. Refusing that by
   default would change the meaning of a documented endpoint, so in 1.x it
   is opt-in and the demo enables it. Making it the default is recorded as
   a 2.0 candidate.

### Demo host hardening

New idempotent script `deploy/demo/ssh-hardening.sh`, written in the style
of `deploy/provision.sh`:

* `/etc/ssh/sshd_config.d/50-wirefan.conf`: `PasswordAuthentication no`,
  `PermitRootLogin no`, `KbdInteractiveAuthentication no`, `MaxAuthTries 3`,
  `AllowUsers <deploy user>`. Validated with `sshd -t` before reload.
* fail2ban jails for `sshd` and for repeated 4xx on `/v1/connect` from the
  Caddy access log.
* `unattended-upgrades` for security updates.
* `deploy/demo/nftables.conf`: per-source-IP new-connection rate and
  concurrent-connection limits on 443 and a SYN meter, set above the
  application's `WIREFAN_IP_CAP` so the app-level cap stays the precise
  control.
* Documentation of the GCP option to replace public 22 with an IAP-only
  firewall rule (source `35.235.240.0/20`) and its convenience cost.

Explicit non-goal, stated in `deploy/demo/README.md`: none of this stops a
distributed flood that saturates the VM's network link.

### systemd sandbox

Add the missing directives to `deploy/wirefan.service`, switch the state
directory to `StateDirectory=wirefan` (with `provision.sh` keeping existing
ownership on upgrade), and record `systemd-analyze security wirefan.service`
before and after in `docs/DEPLOY.md`. `SystemCallFilter=@system-service`
must be validated against the cgo SQLite driver and the Go runtime on a
real Ubuntu 24.04 VM, since a missing syscall shows up as `SIGSYS`.

## Protocol and API changes

| Surface | Change | Default |
| ------- | ------ | ------- |
| Env `WIREFAN_KEY_IP_RATE`, `WIREFAN_KEY_IP_BURST` | New | unset (off) |
| Env `WIREFAN_SIGN_RATE`, `WIREFAN_SIGN_BURST` | New | unset (unlimited) |
| Flag `--sign-same-key-only` | New | off |
| `POST /v1/auth/sign` | `400` for a socket id that is not live; `429` with `Retry-After` when throttled; `403` for a cross-key socket when `--sign-same-key-only` is set | |
| Admin `/v1/keys*` | `429` after repeated failed admin auth from one IP | |
| Metric `wirefan_sign_requests_total{result}` | New | |
| Metric `wirefan_rate_limited_total{scope="key_ip"}` | New label value | |

Example of the per-IP refusal, same shape as today's `RATE_LIMITED`:

```json
{
  "type": "error",
  "code": "RATE_LIMITED",
  "message": "too many requests from this address for this API key",
  "op": "publish",
  "channel": "demo"
}
```

## Compatibility impact

All additions are allowed in a 1.x minor by `docs/COMPATIBILITY.md`: new
env vars, a new flag, a new metric and label value. The new sign responses
cover conditions in which no legitimate app server can be: a socket that is
not live, a flood of bad credentials, an operator-configured rate. The
cross-key refusal, which a legitimate deployment could hit, is opt-in.

2.0 candidate: make `--sign-same-key-only` the default.

## Task breakdown

| Order | Issue | Est. |
| ----- | ----- | ---- |
| 1 | [#43](https://github.com/EthanY33/wirefan/issues/43) | 4 h |
| 2 | [#44](https://github.com/EthanY33/wirefan/issues/44) | 5 h |
| 3 | [#45](https://github.com/EthanY33/wirefan/issues/45) | 6 h |
| 4 | [#47](https://github.com/EthanY33/wirefan/issues/47) | 3 h |
| 5 | [#46](https://github.com/EthanY33/wirefan/issues/46) | 6 h |

Dependencies: issue 3 uses `wirefan_rate_limited_total` from
[#51](https://github.com/EthanY33/wirefan/issues/51).
Issue 1's Hub socket index is reused by the admin kick endpoint in
`03-key-management.md`; issue 3's IP plumbing is reused by the same issue.

## Test plan

* `internal/hub`: `LookupSocket` returns the key for a live socket, and
  nothing after `Remove`; concurrent `Add`/`Remove`/`LookupSocket` under
  `-race`.
* `internal/server/rest_test.go`: sign for a live socket on the same key
  (200), an unknown socket (400), a closed socket (400), a cross-key socket
  with and without `--sign-same-key-only` (403 / 200).
* Sign throttling: 25 bad-credential requests from one IP produce 429 with
  `Retry-After`; requests from another IP are unaffected; X-Forwarded-For
  is honored only from `WIREFAN_TRUSTED_PROXIES`.
* Per-IP sub-limit: two simulated IPs on one key; IP A floods past its
  sub-limit and receives `RATE_LIMITED`, IP B keeps publishing. With the
  sub-limit unset, the existing handler tests pass unchanged. IPv6
  addresses in one /64 share a bucket.
* `internal/server/leak_test.go` still passes; limiter maps are bounded by
  the existing ttl eviction.
* Host: `sshd -t` passes before reload; password and root SSH fail on a
  rebuilt test VM; fail2ban bans after repeated failures; a single-IP
  `cmd/loadtest` run at the documented cap is not dropped by nftables; no
  `SIGSYS` in the journal during a short load run under the new unit.

## Open questions

* Should the per-IP sub-limit default to a fraction of the key budget
  (for example half) once it has run on the demo for a while, instead of
  off? That would be a change of default and so waits for 2.0.
* Is there a deployment that relies on cross-key signing? If none is
  reported before 2.0, the default flips there.
* IAP-only SSH removes direct access from a laptop without `gcloud`. Keep
  public 22 with fail2ban as the documented default and IAP as an option,
  or the reverse?
