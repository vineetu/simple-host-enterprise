# PARITY — Simple Host (hosted) and Simple Host Enterprise

This file is identical in both repos:
[vineetu/simple-host](https://github.com/vineetu/simple-host) (hosted: simple-host.app, and
the small-box edition anyone can run on one ~$5 VPS with Docker Compose and Caddy) and
[vineetu/simple-host-enterprise](https://github.com/vineetu/simple-host-enterprise) (the
Kubernetes edition a company runs behind its own OIDC). They are separate products on purpose.
Parity means shared features and security fixes stay in step, not that the two are identical.

Rules (also in each `CLAUDE.md`):
- Any security fix in one repo is checked against the other the same day, and noted in
  "Security fixes checked across" below.
- A new or changed feature updates this file in the same commit, in both repos (copy it across;
  the weekly box check Signals the owner when the two copies differ or the gap count grows).
- `scripts/check-parity.sh` fails when a numbered `## N. Title` section of that repo's
  `FEATURES.md` has no line in the section index at the bottom.

Status: `same` · `different on purpose` (reason) · `gap → hosted` / `gap → enterprise` (the side
that should get it). Last derived from both `FEATURES.md` files and code on 2026-09-26.

## Feature table

| Area | Hosted | Enterprise | Status |
|---|---|---|---|
| Sites: deploy, versions, rollback, delete | tar.gz/zip or inline JSON files; `KEEP_VERSIONS`; rollback; delete | tar.gz or MCP file list, `If-Match` ETags; 5 versions kept; rollback; delete | `same` |
| Site rename | `PATCH /v1/sites/{s}`, `rename_site` | none | `gap → enterprise` |
| Site export with saved data | `export.tar.gz` (files + state + collections) | version archive download only (files, no saved data) | `gap → enterprise` |
| Per-site hosts `<site>.<owner>.<domain>` | live 2026-09-26; path fallback until the owner's wildcard cert exists | v1.3 2026-09-26; same fallback | `same` |
| Per-owner certificates | root issuer, certbot DNS-01, 40/week 12/day cap | `owner-hosts` reconciler → cert-manager Ingress per owner | `different on purpose` — box vs cluster tooling |
| Small-box path model | `PERSON_HOSTS`/`SITE_HOSTS=off` on event/self-hosted boxes: one shared host, path addresses | n/a (always per-site hosts) | `different on purpose` — a $5 box has no per-owner wildcard DNS/certs |
| Person / owner index page | `<handle>.<domain>/`, public, lists public sites | `<owner>.<base>/`, sign-in required, only listed sites to others | `different on purpose` — company content stays behind company sign-in |
| Free `<name>.<domain>` names | first come, verified at once | none | `different on purpose` — enterprise non-goal: the person's name in the address is the identity |
| Custom domains | CNAME/A bind, 24 h provisional, Caddy on-demand TLS on boxes | none | `different on purpose` — enterprise non-goal (no per-site custom domains) |
| Who can open a site | pages always public; `public`/`unlisted` listing only | five levels `only_me`/`specific`/`company`/`listed`/`network`, admin approval (optionally two) for network, named viewers | `different on purpose` — hosted non-goal: no private pages; enterprise default is only-me |
| Teams | none; one person per account | `team-<name>` namespaces, one role | `different on purpose` — hosted accounts are single people (event participants get their own key) |
| Saved state | one JSON doc; atomic ops (`set`/`inc`/`append`/`remove`/`removeWhere`) + `PUT` with `If-Match`; 1 MB | one JSON doc; last-write-wins or versioned compare-and-set | `different on purpose` — hosted writes need a signed-in visitor or key; enterprise: every viewer is signed in |
| Saved-state history and restore | none | last 20 writes kept, owner or team restores | `gap → hosted` — any signed-in visitor can overwrite state, so it needs the same undo |
| Collections and private collections | append-only lists; private ones on a site's own origin, owner-only read, CSV export | none | `different on purpose` — enterprise decision 2026-09-23: per-site state is the only data store |
| Assets (runtime file uploads) | none | per-site uploads, served at `/_assets/{id}`, counted in quota | `gap → hosted` |
| Malware scan on upload | none | optional clamd, fail closed | `different on purpose` — clamd needs ~1 GB RAM, above the small-box floor |
| Upload validation | `internal/tarball` sanitize, size caps, `blockedExtensions` | same package lineage, same checks | `same` |
| Visitor sign-in on a site | Google or emailed code on the site's own host; host-only cookie + `X-SH-CSRF`; login-CSRF nonce | company OIDC session handed to the site host by a nonce-bound one-time code | `different on purpose` — public visitors vs company identity |
| Owner sign-in and sessions | emailed code or Google → API key kept in the browser; no owner cookie session | company OIDC only; revocable sessions, idle 30m / absolute 8h, sessions page | `different on purpose` — enterprise constraint: the IdP is the only identity |
| API keys: hashing | SHA-256, shown once (2026-09-26) | `shk_`, stored hashed | `same` |
| API keys: expiry | none; rotate replaces all | 90 days default, capped by `API_KEY_MAX_DAYS` | `gap → hosted` |
| API keys: scopes | none (every key is full) | `publish` (default) / `full` / `offboard`, deny-by-default | `gap → hosted` |
| API keys: list and revoke one | none (rotate is all-or-nothing) | mint, list, revoke each | `gap → hosted` |
| MCP connector and OAuth | DCR, PKCE S256, rotating refresh, reuse revokes the grant, hourly sweep, tokens hashed | same design (hosted's adapter was ported from enterprise) | `same` |
| Connector token lifetime and reach | refresh 90 d sliding; Bearer also accepted on `/v1/*` | refresh 30 d from sign-in, TTLs capped; Bearer only on `/mcp` | `different on purpose` — hosted: sign in once and stay signed in, and hand-registered GPT Actions call REST |
| MCP tools | 22 tools, each a REST call | own set incl. teams, viewers, access, state history; each resolves to a route (tested) | `same` — tools follow each side's REST surface |
| Skills and plugin | `website-deploy`, `-builder`, `connect-domain`, `run-hackathon`; Claude + ChatGPT plugins; stale skill → `_notice` | `simple-host`, `simple-host-builder`, `fix-paths-for-subpath-hosting`; `plugin.zip`; obsolete skill → refused | `different on purpose` — each skill teaches its own product; a company can require current skills |
| Audit log | none; server log only | every mutation + visit, hash-chained, `audit-verify`, SIEM stdout stream, export, retention | `different on purpose` — hosted decision 2026-09-05: nothing records an author; enterprise constraint: everything on record |
| Analytics | nginx/Caddy log → views, visitors, local geo; API metrics; `site_analytics` tool | in-app access log → daily views (bots split), downloads, counts only for owners | `different on purpose` — different serving paths; geo is a public-web need |
| Search and company showcase | none; sites are found by link or person page | full-text search of listed sites, `/showcase` | `different on purpose` — enterprise decision 2026-09-23: attribution is the discovery mechanism |
| Quotas | 100 sites per account; per-site archive cap `MAX_ARCHIVE_MB` | per-owner `QUOTA_MAX_SITES` + `QUOTA_MAX_BYTES` (stored bytes), usage on dashboard | `gap → hosted` — a per-owner stored-bytes cap protects a small box's disk |
| Rate limits and abuse caps | per-IP token buckets in memory; size caps; write auth; reserved names | per-pod buckets plus Postgres-shared counters for sign-in, hand-off, key mint, connector | `different on purpose` — one process needs no shared counters |
| Security headers and CSP | `SecurityHeaders`, nonce CSP on apex pages | `security.go`, same approach | `same` |
| Admin | admin key or admin user; usage, bulk participant accounts, delete account, API traffic | IdP admins; disable/enable (revokes sessions, keys, apps), offboard by email, access requests, rankings, export | `different on purpose` — event organiser vs company IT |
| Dashboard | `/dashboard`, owner app, per-site analytics page | `/dashboard`: keys, sites, access, viewers, assets, usage | `same` — each shows its own features |
| AI create and voice input | Grok sidecar only, local speech-to-text | none | `different on purpose` — enterprise: the publisher is the person's own agent via MCP; content stays in the cluster |
| Event / hackathon instances | setup page, participant accounts, `simple-hack.app` names | none | `different on purpose` — this is the small-box edition's job |
| Storage backend | local disk (`DATA_DIR`), served by nginx or Caddy | S3-compatible bucket, pod cache, SSE, optional envelope encryption, retire sweep | `different on purpose` — one folder on one box vs replicas |
| Deployment model | one binary + Postgres + disk: systemd on simple-host.app; `deploy/install/install.sh` + Docker Compose + Caddy for a small box | Kubernetes (kustomize), cert-manager, least-privilege DB role, TLS-only DB and bucket, startup refusals, rollback-safe migrations | `different on purpose` — owner decision: hosted is the small-box edition, enterprise the cluster edition |
| Health and metrics | `/healthz`, `/readyz` | `/healthz`, `/readyz`, `/metrics` on its own port | `different on purpose` — no metrics stack on a small box |
| Static, marketing and legal pages | landing, features, enterprise pages, terms, privacy, support | landing, docs, capabilities, install, changelog | `different on purpose` — public service vs internal install |
| Notifications | none (sign-in email only) | none (SIEM stream only) | `same` |

## Security fixes checked across

| Date | Fixed in | Fix | Other side |
|---|---|---|---|
| 2026-09-26 | hosted | API keys stored only as SHA-256 | enterprise already hashes keys: n/a |
| 2026-09-26 | hosted | visitor Google sign-in bound to the starting browser (login CSRF) | enterprise hand-off already nonce-bound: n/a |
| 2026-09-26 | enterprise | connector tokens accepted only on `/mcp` | hosted keeps Bearer on `/v1/*` on purpose (GPT Actions); see table |

## Section index

Every numbered `FEATURES.md` section, per repo, and the rows above that cover it.

| Repo | FEATURES.md section | Rows |
|---|---|---|
| hosted | Sites and deploy | Sites; Site rename; Site export; Upload validation |
| hosted | Per-site and per-person addresses, and legacy redirects | Per-site hosts; Per-owner certificates; Small-box path model |
| hosted | Claimed `<name>.simple-host.app` and custom domains | Free names; Custom domains |
| hosted | Saved state (shared JSON per site) | Saved state; Saved-state history |
| hosted | Collections, including private collections | Collections and private collections |
| hosted | Visitor sign-in (Google, emailed code) | Visitor sign-in on a site |
| hosted | Owner auth (API keys, email codes, profile) | Owner sign-in; API keys rows |
| hosted | MCP connector and OAuth (chat apps) | MCP connector; Connector token lifetime |
| hosted | Skills and plugin distribution | Skills and plugin |
| hosted | Owner dashboard and owner app | Dashboard |
| hosted | Admin (operator) | Admin |
| hosted | Analytics and geo | Analytics |
| hosted | Showcase / person index | Person / owner index page |
| hosted | AI create (Grok sidecar) and voice input | AI create and voice input |
| hosted | Hackathon / event instances (simple-hack.app) | Event / hackathon instances; Small-box path model |
| hosted | Enterprise and marketing pages | Static, marketing and legal pages |
| hosted | Legal and support pages | Static, marketing and legal pages |
| hosted | Abuse limits and hardening | Rate limits and abuse caps; Quotas; Security headers |
| hosted | Signals and notifications | Notifications |
| hosted | Operations (health, schema, CLI) | Health and metrics; Deployment model |
| hosted | MCP tool index (`internal/mcp/tools.go`, 22 tools) | MCP tools |
| hosted | Unplaced routes and tools | (index of FEATURES itself, no feature) |
| enterprise | Identity: OIDC sign-in, sessions, hand-off | Owner sign-in and sessions; Visitor sign-in on a site |
| enterprise | API keys (CI and automation) | API keys rows |
| enterprise | MCP server, OAuth connector, plugin.zip | MCP connector; Connector token lifetime; Skills and plugin |
| enterprise | Skills bundle and skill-version gate | Skills and plugin |
| enterprise | Sites: deploy, versions, rollback, delete | Sites; Site rename; Site export; Per-site hosts; Quotas; Malware scan |
| enterprise | Bucket storage, cache, retire sweep, migrate-storage, restore and reencrypt | Storage backend |
| enterprise | Access levels and network approval | Who can open a site |
| enterprise | Named viewers (restricted sites) | Who can open a site |
| enterprise | Teams | Teams |
| enterprise | Saved state and its history | Saved state; Saved-state history |
| enterprise | Assets (uploaded files) | Assets |
| enterprise | Audit log, access log, export, retention | Audit log |
| enterprise | Admin page | Admin |
| enterprise | Dashboard | Dashboard |
| enterprise | Search, showcase, owner index | Search and company showcase; Person / owner index page |
| enterprise | Metrics, health, request log, rate limits | Health and metrics; Rate limits and abuse caps; Analytics |
| enterprise | Landing and static pages | Static, marketing and legal pages |
| enterprise | Configuration, database roles and startup refusals | Deployment model |
| enterprise | Owner hosts: per-owner certificates | Per-owner certificates; Per-site hosts |
