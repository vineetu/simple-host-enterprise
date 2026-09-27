# Server and addresses

An install lives at one address on its own registrable domain, like `sites.example.com`
(never a subdomain of the company's main domain, so a published page can never read the
company's cookies). Each person or team gets `<owner>.<host>` and every site its own origin,
`<site>.<owner>.<host>`. One wildcard DNS record covers every depth.

**Per-owner certificates.** Each owner needs a `*.<owner>.<host>` certificate. With
`OWNER_CERTS=auto`, the owner-hosts reconciler asks cert-manager for it through
`OWNER_CERT_ISSUER` (an internal CA issuer, recommended, or ACME with DNS-01), and an owner's sites
move to their own hosts once it is ready; until then they are served at `<owner>.<host>/<site>/`.

**The proxy in front.** Set `TRUSTED_PROXY_CIDRS` to the ingress controller's pod range, so rate
limits and the audit log see the real client address.

<!-- settings:group=server -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `PUBLIC_BASE_URL` | none | text | The address people open, like https://sites.example.com: its own registrable domain, never a subdomain of the company's main one. Every site is served at <site>.<owner>.<host>. **Required.** |
| `SECURE_MODE` | `false` | `true` / `false` | true requires HTTPS for PUBLIC_BASE_URL and a separate redirect port. Every real install sets it. **Security-sensitive.** |
| `PORT` | `8080` | 1–65535 | The port the server listens on. |
| `HTTPS_REDIRECT_PORT` | `8081` | 1–65535 | The port that redirects plain HTTP to HTTPS. Must differ from PORT with SECURE_MODE. |
| `TRUSTED_PROXY_CIDRS` | `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7` | text | The proxies in front of the server (the ingress controller's pods), so the client address comes from X-Forwarded-For. Empty trusts none. **Security-sensitive.** |
| `OWNER_CERTS` | `auto` | `auto` / `manual` | auto: each owner's sites move to their own hosts once the owner-hosts reconciler has their certificate. manual: you issue those certificates yourself. |
| `OWNER_CERT_ISSUER` | none | text | The cert-manager ClusterIssuer that signs each owner's *.<owner>.<host> certificate (an internal CA, or ACME with DNS-01). Needed by the reconciler. |
| `OWNER_INGRESS_TEMPLATE` | `simple-host` | text | The install's own Ingress, which each owner's Ingress copies its class and annotations from. |
| `OWNER_HOSTS_INTERVAL` | `15s` | Go duration | How often the reconciler applies owner Ingresses and records certificate readiness. |
| `RESERVED_LABELS` | none | text | Extra hostnames (comma-separated) no person or team may take as a name, on top of the built-in ones. |
<!-- /settings -->

## Recipes

**Internal CA.** Create a cert-manager `ClusterIssuer` backed by your internal CA and set
`OWNER_CERT_ISSUER` to its name. No public rate limits apply.

**Reserve hostnames.** `RESERVED_LABELS=intranet,vpn,hr` keeps those names from ever becoming a
person's or team's address.
