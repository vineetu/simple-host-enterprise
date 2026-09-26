# Changelog

Releases are published as `ghcr.io/vineetu/simple-host-enterprise:<version>`;
pin the digest, not the tag. `simple-host version` prints the running
release, commit and schema.

## v1.3.1 — 2026-09-26

No schema change (still 0042); rolling back to v1.3.0 is safe. Skills are at
0.13.1 (0.11.0 still works).

### Operations
- `simple-host restore` into a site that does not exist yet applies the same
  name rules as creating a site through the API (lowercase letters, numbers
  and hyphens, at most 63 characters, not `xn--`, not reserved, not another
  site's derived address), so a restore never creates a site with no address.
  Restoring into an existing site is unchanged.

### Skills and served text
- Skills 0.13.1 correct stale text: a `428` means send `If-Match` on the
  owner-qualified routes; `/api/me`'s `usage` is one entry per namespace;
  the 10-team limit applies to creating a team, and a team holds at most 50
  members; team member search marks existing members `already_member`; an
  asset is served at the `url` its upload returns; a `403` with `scope`
  means the key's scope does not allow the call; a framework's base path is
  relative (`./`), never `/<site>/`.
- MCP tool text no longer mentions publishing to a site shared with you
  (viewers never publish) and says when `etag` is required.
- `openapi.yaml`: redirect codes, which rate limits are shared across
  replicas, the full upload denylist, two-admin approval, API key scopes,
  the `url` field on a site, and `/api/search` requiring sign-in, all as the
  server behaves.
- The home page, capabilities page and empty showcase no longer say sites
  are unlisted by default, served from a subpath, or keep exactly five
  versions.

### Docs
- Install, configuration, storage, security review and feature docs brought
  in line with v1.3.0: audit writes that are transactional, envelope key
  rotation order, `OWNER_CERTS=manual` steps, `BACKUP_STORAGE_REGION`
  (optional), the `--` rule (account and team names only), site hosts in
  the threat model, and every subcommand. `docs/page/` is marked historical.

### Local evaluation
- `make local` no longer stalls on MinIO. MinIO stopped publishing community
  images and `quay.io/minio/*` now needs a login, so the in-cluster MinIO
  (`deploy/components/minio`) runs `pgsty/minio`, a maintained build of the
  same MinIO source on Docker Hub, pinned by digest. The bucket Job uses the
  `mc` client from the same image. Nothing else changes: same credentials,
  KMS key, health checks and versioned bucket.

## v1.3.0 — 2026-09-26

Schema 0042. Migrations 0041 (renames team rows) and 0042 (adds the
`owner_hosts` table) are both marked backward-compatible, so rolling back to
v1.2.1 is safe; after a rollback sites are served at `<owner>.<base>/<site>/`
again and old team addresses stop redirecting. Skills are at 0.13.0 (0.11.0
still works).

### Sites
- Every site now has its own origin, at every access level: it is served at
  the root of `<site>.<owner>.<base>/`, for example
  `todo.alice.example.com/`. An owner's sites no longer share cookies,
  storage or scripts with each other. The first visit to each site hands
  the session over without a prompt, as restricted sites already did.
- Until an owner's certificate is ready, their sites are served at
  `<owner>.<base>/<site>/` as before, and responses give that address. Once
  it is ready, `<owner>.<base>/<site>/...` and `/api/sites/<site>/...` on the
  owner host redirect to the site's own host with path and query kept (301
  for GET and HEAD, 308 otherwise). `<owner>.<base>/` keeps the person's or
  team's index page.
- A site shared with named people in v1.2 lived at `<owner>--<site>.<base>`;
  that address now redirects to the site's current one.
- New site names are lowercase letters, numbers and hyphens, start and end
  with a letter or number, are at most 63 characters and do not begin with
  `xn--`; the name becomes the address. Otherwise the create is refused with
  400 `invalid_site_name`. Existing sites keep their names; one whose name
  is not a valid address gets a derived one ending in six hex digits, and a
  new name equal to that derived address is refused with 409
  `name_conflict`. Always use the `url` a response gives.
- Search results indexed before v1.3 carry the old addresses, which
  redirect; each site's next deploy reindexes it.
- Admin ranking cards link to each site's own address.

### Certificates
- A TLS wildcard covers one label, so each owner needs a `*.<owner>.<base>`
  certificate. A new Deployment, `simple-host-owner-hosts`
  (`deploy/components/owner-hosts`, the same image running
  `simple-host owner-hosts`), keeps one Ingress per owner with sites,
  annotated for cert-manager, and records in `owner_hosts` when that
  owner's certificate is Ready. The server moves an owner to site hosts
  within about 15 seconds of that.
- It runs apart from the server so the server's pods still hold no
  Kubernetes credential. Its Role allows Ingresses and reading Certificates
  in the install's namespace; it never reads Secrets.
- New settings: `OWNER_CERTS` (`auto`, the default, or `manual`),
  `OWNER_CERT_ISSUER`, `OWNER_INGRESS_TEMPLATE`, `OWNER_HOSTS_INTERVAL`
  (`docs/configuration.md`). With `manual`, leave the component out and
  provide each owner's certificate and ingress rule yourself.

### Teams
- Team names begin with `team-`. Creating `sales` or `team-sales` both make
  `team-sales`, and the team routes accept either spelling. A person's
  sign-in name never begins with `team-` (`team-alpha` becomes `teamalpha`).
- Migration 0041 renames existing teams `sales` to `team-sales`. It stops,
  naming them, if a renamed team would take an existing account's address
  or run past 58 characters; delete the team or remove the account, then
  run it again.
- Old team addresses (`sales.<base>/...`, and `sales--<site>.<base>`)
  redirect to the `team-sales` ones for as long as no account holds the
  name `sales`.

### Upgrade notes
- DNS: no change. The one `*.<base>` record already answers for names at
  any depth, including `todo.alice.<base>`.
- Certificates: install cert-manager and a ClusterIssuer (an internal CA is
  best; Let's Encrypt limits certificates per domain per week), set
  `OWNER_CERT_ISSUER` to its name, and include
  `deploy/components/owner-hosts` in your overlay. On a private registry,
  add the pull secret to the new `simple-host-owner-hosts` ServiceAccount.
  `INSTALL.md`, "Site addresses", has the steps and the manual option.
- Sites keep serving at `<owner>.<base>/<site>/` until each owner's
  certificate is ready, so nothing breaks while certificates are issued or
  if the component is not yet deployed.
- Bookmarks and shared links redirect.
- On a site's own host the path is the site's own file path, so a page that
  requests `/<site>/...` from its own origin gets a 404 there. Relative
  paths, which the skills have always asked for, work at both addresses.
- Proven end to end on kind with ingress-nginx and cert-manager and a CA
  ClusterIssuer: fallback, certificate issue, switch to site hosts,
  redirects, and cleanup when an owner's last site is deleted.
- Automatic owner certificates are tested only with an in-cluster ingress
  controller (ingress-nginx); AWS ALB (ACM certificates only, and one load
  balancer per Ingress without a shared `group.name`) and GKE's `gce`
  Ingress (one load balancer per Ingress) need an in-cluster controller for
  the site hosts, or `OWNER_CERTS=manual`.

## v1.2.1 — 2026-09-26

Schema 0040. Migration 0040 only adds a read-only function and is marked
backward-compatible, so rolling back to v1.2.0 is safe. Skills are at 0.12.1
(0.11.0 still works).

### Sign-in
- With `OIDC_ISSUER=https://accounts.google.com`, the server refuses to
  start while `ALLOWED_EMAIL_DOMAINS` is empty: any Google account could
  otherwise sign in. Set it before upgrading a Google install. Other issuers still start with it empty (an Okta org or
  a single-tenant Entra ID issuer is already your own) and log a reminder.
- INSTALL.md, "Internal-only installs": how to narrow `OAUTH_REDIRECT_HOSTS`
  (whose default admits `chatgpt.com` and `claude.ai`) when the install is
  not reachable from the internet.

### Keys
- `GET /api/audit` and `GET /api/access` give the company-wide view only to
  an admin's browser session. An admin's API key (any scope) or connected
  app now sees what anyone else sees: their own namespace and their teams'.
- Skills 0.12.1 ask for a **Publish** key by default, and for **Full** only
  when the user wants a management action a publish key cannot do.

### Audit
- Each stdout audit line now carries its row's chain `seq` and `hash`, and
  is written only after the transaction that recorded it commits, so the
  SIEM never receives a rolled-back event and holds an anchor for every
  event it received. `audit-verify -expect SEQ:HASH` with any SIEM line's
  values proves the database still agrees; a database owner who rewrites
  rows and recomputes the chain is caught that way (docs/configuration.md).
  Migration 0040 adds `audit_chain_entry`, which lets the server's role
  read one event's seq and hash and nothing else of the chain.
- `audit-verify` recomputes each event's canonical form and SHA-256 in Go
  instead of calling the database's `audit_event_canonical`, so redefining
  that function no longer hides a rewritten row.

### Uploads and serving
- A deploy is refused if it contains Windows or other executables,
  installers, packages or disk images, in addition to the source scripts
  refused before: `.exe .dll .msi .msix .appx .scr .com .pif .cpl .hta .vbs
  .vbe .jse .wsf .wsh .lnk .reg .jar .apk .aab .pkg .deb .rpm .iso .img`.
  `.js`, ZIP and DMG are still served. The list is in
  `docs/configuration.md`; `CLAMD_ADDR` is the scan for the bytes.
- Cache fills no longer read a whole version into memory: a plain object
  streams from the bucket to a temporary file on the cache volume. An
  envelope-encrypted object is still read whole (its AES-GCM tag covers the
  whole body) but decrypted in place, and at most 1 GiB of such bodies are
  held at once per pod; further fills wait. Leave room on the cache volume
  for one compressed archive per fill in progress (docs/storage.md).

### CI
- CI runs the whole test suite against a real Postgres, including the
  migration, grants, audit trigger and least-privilege tests, and fails if
  any of them skip. `make test-db` runs the same thing locally.

## v1.2.0 — 2026-09-26

Schema 0039. Migrations 0035–0039 are additive and marked
backward-compatible, so rolling back to v1.1.3 is safe; on v1.1.3 every API
key acts with its old, unscoped reach, the audit chain keeps growing but is
not trimmed by v1.1.3's `prune`, and quotas, two-admin approval and the
shared rate limits are not enforced. Skills are at 0.12.0 (0.11.0 still
works).

### Sessions and leavers
- New defaults: `SESSION_TTL=8h`, `SESSION_IDLE=30m` (were `12h`/`1h`).
  Hard caps: `SESSION_TTL` at most `24h`, `SESSION_IDLE` at most `8h` and
  never longer than `SESSION_TTL`; out of range refuses startup. Set them to
  match your IdP's session policy.
- The OAuth connector's token lifetimes are configurable:
  `OAUTH_ACCESS_TTL` (default `1h`, at most `24h`) and `OAUTH_REFRESH_TTL`
  (default `720h`, at most `2160h`). The refresh lifetime now counts from the
  sign-in that connected the app; rotating the refresh token no longer
  extends it, so a connected app re-authenticates at the IdP at least that
  often.
- Offboarding: `POST /api/admin/users/disable` with `{"email": ...}`
  disables a person (sessions, API keys and connected apps end at once) for
  HR automation, callable by an admin's session or by an `offboard`-scoped
  key. Audited. INSTALL.md, "Sessions and leavers": disable in the IdP
  **and** in Simple Host.

### Scoped tokens
- API keys carry a scope chosen at mint: `publish` (default: deploy,
  update, roll back, versions, saved data, assets, `/api/me`, `/mcp`),
  `full` (everything the person can do except administration) or `offboard`
  (admins only; the disable route and nothing else). The dashboard shows
  each key's scope. Keys that existed before v1.2 became `full`; no key of
  any scope reaches `/api/admin/*` except `offboard` on its one route.
- OAuth connector tokens (`Authorization: Bearer`) work only on `/mcp` and
  the calls it makes for its tools; direct REST calls with them are refused.

### Audit
- `audit_events` is hash-chained inside Postgres (`audit_chain`, 0036), under
  a lock so replicas cannot fork it. `simple-host audit-verify
  [-expect SEQ:HASH]` walks the chain and reports the first break.
  `simple-host prune` trims the chain with the partitions it drops. The
  access log is not chained (docs/security-review.md, 2(e)).
- In a rollout where v1.1.3 and v1.2 pods (or a v1.1.3 `prune` job) run side
  by side, run `audit-verify` only after the first v1.2 `prune` has run:
  v1.1.3's `prune` drops audit partitions without trimming the chain, which
  `audit-verify` would report as a break until v1.2's `prune` trims it.
- Every audit event is also written to stdout as one JSON line with
  `"type":"audit"`, for any cluster log shipper to forward to a SIEM
  (docs/configuration.md, "Streaming the audit log to a SIEM"). The line is
  written off the request's goroutine; if stdout stalls and the buffer fills,
  lines are dropped and counted in `simplehost_audit_stream_dropped_total`.
- Sign-in, session revoke, API key mint and revoke, and the connector's
  sign-in and revoke now commit their audit row in the same transaction as
  the change. The remaining best-effort writes retry, survive a client
  disconnect, and log `AUDIT WRITE FAILED` if they still fail; they are
  listed in docs/security-review.md.

### Uploads
- Per-owner quotas (a person's or a team's namespace), checked in the deploy
  and asset-upload transactions: `QUOTA_MAX_SITES` (default 1000, `409`),
  `QUOTA_MAX_BYTES` (default 10 GiB across every kept version and asset,
  `413`), `QUOTA_MAX_VERSIONS` (default 5 per site, as before; older
  versions go to the retire queue). The dashboard shows usage against them.
  Existing versions' sizes are filled in from the bucket in the background
  (`versions.size_bytes`, 0037); until that fill finishes on a large
  install, a version whose size is not yet recorded does not count toward
  `QUOTA_MAX_BYTES`.
- Optional malware scan: with `CLAMD_ADDR` set, every file of a deploy and
  every uploaded asset is streamed to clamd before anything is stored; an
  infected upload is refused with `422`, and an unreachable scanner refuses
  uploads with `503`. Unset, nothing is scanned.

### Storage
- `simple-host reencrypt` rewrites every stored object under the first
  `BACKUP_ENVELOPE_KEY` in the key-bound form: idempotent, resumable,
  verified by reading back, with progress output. Old envelope keys can be
  removed after a clean run; it also moves a plaintext install onto the
  envelope. Procedure: docs/storage.md, "Rotating the envelope key".

### Access
- `NETWORK_ACCESS_APPROVALS` (default `1`; `2` = two different admins must
  approve a network access request). In every mode the requester can never
  approve their own request, even as an admin. `/admin` and the owner's
  view show approvals so far; each approval is audited
  (`network_access_approvals`, 0038).

### Rate limits
- Sign-in, the OIDC callback, session hand-off, API key mint and the
  connector's token and registration endpoints are counted in Postgres
  (`rate_limit_counters`, 0039, hashed keys), so every replica shares one
  budget; they fall back to the per-pod limit if the database does not
  answer. Every other limit stays per pod (docs/install.md, "Rate limits and
  replicas").

## v1.1.3 — 2026-09-26

Schema 0034, marked backward-compatible: it drops the unused tables
`reset_requests`, `key_reissues` and `ai_usage`, which v1.1.2
never reads or writes, so rolling back to v1.1.2 is safe.

### Security
- The OIDC issuer, and the authorization, token and JWKS endpoints its
  discovery document names, must be `https://`. Plain `http` is refused at
  startup unless `OIDC_INSECURE_ALLOWED=true` (the local overlay's
  in-cluster Dex sets it; never on a real install).
- `migrate` sets the application role's password as a SCRAM-SHA-256 hash
  computed in the binary, so the plaintext `DB_APP_PASSWORD` never reaches
  Postgres statement logs, pgaudit or `pg_stat_statements`. The password
  must be printable ASCII (the generated one is hex).
- The client-side envelope (`BACKUP_ENVELOPE_KEY`) now binds each object to
  its own key: one site's object copied over another's no longer decrypts.
  Objects written by v1.1.0 to v1.1.2 are still read (logged once per
  start); every new deploy, upload and restore writes the bound form.
  `restore` re-encrypts the copy instead of copying on the server side.
- With the envelope on, an unenveloped object is refused. An install that
  added `BACKUP_ENVELOPE_KEY` after it already held sites sets
  `BACKUP_ENVELOPE_PLAINTEXT_ALLOWED=true` before upgrading.
- Assets are checked against their recorded SHA-256 before they are served.
- A malformed `SESSION_SIGNING_KEY` or `BACKUP_ENVELOPE_KEY` entry is no
  longer echoed in the startup error.
- A failed OIDC token exchange logs the status and the provider's error
  code only, not the response body.
- The `migrate` init container and the `prune` job get only the database
  keys from the Secret, not every secret.
- `prune` deletes sessions that ended more than `ACCESS_LOG_RETENTION_DAYS`
  ago (with their IP and user agent) and hand-off codes older than a day.

### Install
- Database storage encryption at rest is listed as a requirement, with the
  per-cloud setting (AWS: `--storage-encrypted` at creation).
- The install runbook generates `BACKUP_ENVELOPE_KEY` by default and says
  to escrow it before first use: losing it loses every site.
- The runbook no longer passes the database owner password on the command
  line.

### Removed
- `POST /api/admin/classify-sites` and the site-type classifier: no
  classifier was ever wired, so the route answered 503 and no site got a
  type. The showcase's type chips and `?type=` filter go with it. The
  `sites.site_type` columns stay one more release for rollback.
- The `POST /api/auth` and `POST /api/reset-requests` placeholders (they
  now answer 405 like any unknown POST).
- `GET /api/admin/access-requests`, a JSON list nothing called; `/admin`
  shows the same requests and keeps approve, decline and revoke.

### Development
- `FEATURES.md` maps every feature to its routes, MCP tools, skill text,
  pages, code, tables and config. `make test` and CI fail when a registered
  route or MCP tool is missing from it.

## v1.1.2 — 2026-09-25

No schema change (still 0033).

### Operations
- A bucket fault no longer takes pods out of rotation. v1.1.1 failed
  `/readyz` when the bucket could not be read; every pod shares the bucket,
  so one credential problem took the whole instance to 503, cached pages
  included. Now `/readyz` fails only on the database and schema. While the
  bucket is failing, cached pages keep serving; uncached pages answer 503
  and publishing fails until it is fixed.
- New metric `simplehost_bucket_ok` (1/0) and log line `readyz: bucket: ...`
  for the bucket check: alert on them. Readiness failures are now labelled
  by cause (`database:` or `schema:`); v1.1.1 labelled every one
  `database:`.

## v1.1.1 — 2026-09-25

Fixes from a real v1.0.0 → v1.1.0 upgrade on a managed cluster. No schema
change (still 0033).

### Operations
- Rolling updates no longer drop requests: each pod waits 10 seconds after
  it is told to stop (a `preStop` sleep) so the ingress stops sending it
  traffic first. **Needs Kubernetes 1.30 or later.**
- The two replicas run on different nodes whenever there are two or more
  nodes, also after rollouts and node drains.
- `/readyz` now fails when the bucket credentials can no longer read
  objects, not only when the bucket is unreachable.

### Upgrading from v1.0.x
- The storage migration now has a dry run that changes nothing, and
  `migrate-storage -dry-run` runs against the v1.0.x schema, before any
  migration is applied. The v1.1.0 steps ran migrations during the dry run.
- [docs/storage.md](docs/storage.md) gives the database backup commands,
  says which step applies the migrations, and gives lifecycle-rule commands
  for S3 providers other than AWS.
- Database sizing counts the extra pod a rollout starts: plan
  `max_connections` for (replicas + 1) × 20 plus the jobs.
- `TRUSTED_PROXY_CIDRS`: take the range from the ingress controller's pod
  addresses; a node's pod CIDR is not always it.

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
- Any team member can leave. When the last active member leaves, the team
  and its sites are deleted once the team's name is typed back; deleting a
  team deletes its sites the same way. An admin can delete a team whose
  members are all disabled.
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
