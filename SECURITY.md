# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 1.x | Yes |
| 0.x | No. Upgrade to the latest 1.x release. |

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub's
[private vulnerability reporting](https://github.com/EthanY33/wirefan/security/advisories/new)
for this repository. Include the wirefan version (`wirefan --version`), how
the server was started (flags and `WIREFAN_*` environment variables, with
secrets removed), and the smallest reproduction you have.

You can expect an acknowledgement within a week. Confirmed issues are fixed
on `main`, released as a patch version, and credited in the release notes
unless you ask otherwise.

## Scope

In scope: the `wirefan` server binary, the wire protocol and HTTP API it
serves, the `@wirefan/client` package in `clients/js`, and the scripts under
`deploy/`.

Things that are working as designed, documented in
[`docs/DESIGN.md`](docs/DESIGN.md) and [`docs/PROTOCOL.md`](docs/PROTOCOL.md):

- Public channels are readable by any client holding a valid API key id.
  Use `private-*` or `presence-*` channels, which require an HMAC-signed
  subscribe token, for anything that needs authorization.
- The admin listener (`/v1/keys`, `/metrics`, `/debug/pprof/*`) binds to
  `127.0.0.1:6060` by default. Exposing it publicly is an operator choice.
- `X-Forwarded-For` is honored only from addresses listed in
  `WIREFAN_TRUSTED_PROXIES`.
