# Observability

- **Probes:** `/healthz` (liveness) and `/readyz` (database reachable, schema current, bucket
  answering).
- **Metrics:** Prometheus at `/metrics` on its own port, not exposed by the Service or Ingress:
  requests, latency, the database pool, the running release, bucket health, startup warnings and
  owners waiting for a certificate ([../install.md](../install.md), section 12).
- **Audit log:** every change, hash-chained (`simple-host audit-verify` checks it), and streamed
  as one JSON line per event to stdout for your SIEM.
- **Startup log:** a `WARNING` for a rate limit set far looser than built in, or a
  `RATE_LIMIT_*` name the server does not have.

<!-- settings:group=observability -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `METRICS_PORT` | `9090` | 1–65535 | The port of the separate /metrics listener (Prometheus); not exposed by the Service or Ingress. |
<!-- /settings -->
