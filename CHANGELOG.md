# Changelog

Releases are published as `ghcr.io/vineetu/simple-host-enterprise:<version>`;
pin the digest, not the tag. `simple-host version` prints the running
release, commit and schema.

## v1.1.0 — 2026-09-25

### Storage
- Site files now live in the S3-compatible bucket, which is the only copy.
  Each pod keeps a small local cache, so the Deployment runs 2 replicas and
  any replica can serve any site.
- `simple-host migrate-storage` copies site files from the old volume into
  the bucket (see Upgrading).

### Signing in from AI apps
- AI apps connect to `/mcp` with the company's own sign-in (OAuth through
  your OIDC provider); nobody pastes an API key into a chat.
- Each installation offers its own plugin download, already pointed at that
  installation. Skills are at 0.11.0.

### Who can open a site
- Five access levels per site: Only me, Specific people, Company (anyone
  signed in with the link), Listed (also in the showcase and search), and
  Network (anyone who can reach the server, no sign-in). New sites start at
  "Only me".
- Network access is requested by the owner and granted by an admin.
- Per-site editors are removed; to work on a site together, use a team.
- Saved data keeps its last 20 versions, and the owner can restore one.
- Owners see how many people visited their site; admins see who and when.

### Security
- Session cookies are bound to the host that issued them, and hand-off codes
  are spent on the first attempt, successful or not.
- Sign-in requires a verified email from the identity provider.
- API keys (for CI and automation) start with `shk_` and expire: 90 days by
  default, never more than 365 (`API_KEY_MAX_DAYS`).
- The client address comes from `TRUSTED_PROXY_CIDRS` only, so rate limits,
  logs and audit rows cannot be spoofed with a forged header.
- The server connects as a least-privilege database role and refuses to start
  on a role that could alter the audit trail.
- Audit: server-side timestamps, IP and User-Agent on every row, sign-in
  failures and access denials recorded.
- A restricted site's saved-data API is reachable only on that site's own
  address.
- Sites cannot register service workers.
- CSV exports neutralise spreadsheet formulas.

### Operations
- Release images are signed with cosign (keyless, bound to the release
  workflow), carry an SBOM and build provenance, and no `latest` tag is
  published.
- Prometheus metrics on port 9090 (`/metrics`), not exposed by the Service.
  `/readyz` caches its database check for 10 seconds and logs why it fails.
- Each migration and its record commit in one transaction; the migrator waits
  a bounded time for its lock and runs with lock and statement timeouts.
- A migration can be marked backward-compatible, and the server then starts
  against it, so an image can be rolled back without restoring the database.
- `make preflight` refuses the in-cluster evaluation Postgres outside the
  local overlay, and the server warns at startup when it runs on it.
- Install docs per cloud: AWS, Azure, GCP, OCI and UpCloud.

### Upgrading from v1.0.x
- **Migration 0033 is not backward-compatible**: it removes per-site editors.
  Once it has run, going back to v1.0.x needs a database restore from before
  the upgrade. Take a backup first.
- Existing sites keep who could open them: a site with named viewers becomes
  Specific people, a public site Listed, anything else Company.
- Skills older than 0.11.0 are refused; reinstall the plugin from your
  installation.
- Run `simple-host migrate-storage` once, before the new pods serve, to move
  site files from the old volume into the bucket. Keep the volume until the
  new release is verified. The steps are in
  [docs/storage.md](docs/storage.md).

## v1.0.0 — 2026-09-24

First release.
