# deploy/demo

What the public demo at <https://wirefan.ethanyucetepe.dev> runs on top of
the standard setup in [`docs/DEPLOY.md`](../../docs/DEPLOY.md). None of it is
needed for a private deployment. It exists because the demo's API key is
public, so anyone can make the server fan out traffic, and cloud egress is
billed per byte.

The host is a Google Cloud e2-micro in `us-east1` on the **Standard**
network tier with a reserved static IP and **no service account**. The two
controls below are written for that host (interface `ens4`, the GCE metadata
server); adjust both for anything else.

| File | Installs to | What it does |
|---|---|---|
| `egress-shape.service` | `/etc/systemd/system/` | Caps egress on `ens4` at 550 kbit/s with a `tbf` qdisc. That is at most ~184 GB in a 31-day month, under the 200 GB of Standard Tier egress Google includes free each month in a region, so the worst case costs nothing no matter what visitors do. |
| `egress-guard` | `/usr/local/sbin/` | Backstop, run every 15 s. Puts the rate cap back if it is missing, and once this billing month's egress (Pacific time, as Google bills) passes the `egress-cap-gb` metadata attribute (default 175 GiB) stops caddy and wirefan until the next month. With the rate cap in place it never trips. |
| `egress-guard.service`, `egress-guard.timer` | `/etc/systemd/system/` | Run the guard. |

Install, as root on the host:

```bash
install -m 0755 egress-guard /usr/local/sbin/egress-guard
install -m 0644 egress-shape.service egress-guard.service egress-guard.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now egress-shape.service egress-guard.timer
```

The demo's Caddyfile also sends the bare URL to the page with the public key,
because the page needs `?key=` to connect and the bare domain is what people
type from a resume. Add this inside the site block, above `reverse_proxy`
(`provision.sh` rewrites the Caddyfile, so re-add it after re-provisioning):

```caddyfile
@bare {
    path /
    not query key=*
}
redir @bare /?key=<public demo key id> 302
```

The demo also sets `WIREFAN_IP_CAP=60` in `/etc/wirefan/env`. Its page weighs
about 20 KB compressed, so the rate cap still loads it in well under a second.
