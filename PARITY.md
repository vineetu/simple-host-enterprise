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
that should get it). Last derived from both `FEATURES.md` files and code on 2026-09-27.

## Feature table

| Area | Hosted | Enterprise | Status |
|---|---|---|---|
| Sites: deploy, versions, rollback, delete | tar.gz/zip or inline JSON files; `KEEP_VERSIONS`; rollback; delete | tar.gz or MCP file list, `If-Match` ETags; 5 versions kept; rollback; delete | `same` |
| Recently deleted (undo a delete) | 7 days: delete takes a site offline and keeps it whole (name, saved data, collections, versions, claimed names held); `GET /v1/me/deleted-sites`, `POST /v1/sites/{s}/restore`, `list_deleted_sites`/`restore_site`, owner app; counts toward the site cap; hourly purge (2026-09-27) | 30 days: whole site (files, saved data, access, viewers, assets) restorable by owner, team member or admin (also after an admin's "Delete sites" for a leaver); name held against create, rename and hand-over; counts toward quota; purged by the sweeper | `same` — windows differ on purpose: a small box's disk vs the bucket's 30-day noncurrent-version retention |
| Site rename | `PATCH /v1/sites/{s}`, `rename_site`; old address 404s | `POST .../rename`, `rename_site`, dashboard; old address redirects until the name is reused | `gap → hosted` — the old address should redirect |
| Hand a site to another owner | none | `POST .../transfer`, `transfer_site`, dashboard: owners and members move a site only into a team they are in (never to a person); admin moves a leaver's or abandoned team's sites to a team or person; network access dropped on a move; audited in both namespaces; old address redirects only for people who can open the site | `different on purpose` — hosted has no teams or company leavers |
| Change a person's address (handle) | self-serve `PATCH /v1/me` and owner app: free before publishing, then once per 30 days; old handle kept as an alias so every old address redirects; new handle's certificate requested (2026-09-27) | none; the username label comes from the IdP | `gap → enterprise` — an admin rename (with redirects) for when a directory name changes |
| Site export with saved data | `export.tar.gz` (files + state + collections); `export_site` tool and a 10-minute signed download link for keyless (connector) users | version archive download only (files, no saved data); an admin's export of a disabled person carries saved data and its history | `gap → enterprise` |
| Per-site hosts `<site>.<owner>.<domain>` | live 2026-09-26; path fallback until the owner's wildcard cert exists | v1.3 2026-09-26; same fallback | `same` |
| Per-owner certificates | root issuer, certbot DNS-01, 40/week 12/day cap | `owner-hosts` reconciler → cert-manager Ingress per owner | `different on purpose` — box vs cluster tooling |
| Small-box path model | `PERSON_HOSTS`/`SITE_HOSTS=off` on event/self-hosted boxes: one shared host, path addresses | n/a (always per-site hosts) | `different on purpose` — a $5 box has no per-owner wildcard DNS/certs |
| Person / owner index page | `<handle>.<domain>/`, public, lists public sites | `<owner>.<base>/`, sign-in required, only listed sites to others | `different on purpose` — company content stays behind company sign-in |
| Free `<name>.<domain>` names | first come, verified at once | none | `different on purpose` — enterprise non-goal: the person's name in the address is the identity |
| Custom domains | CNAME/A bind, 24 h provisional (not while DNS points here), certificate issued automatically (HTTP-01 issuer on prod; Caddy on-demand TLS on boxes), old address kept until the new one is live, lapsed domains let go after 72 h, released free names stay with their site | none | `different on purpose` — enterprise non-goal (no per-site custom domains) |
| Who can open a site | pages always public; `public`/`unlisted` listing only | five levels `only_me`/`specific`/`company`/`listed`/`network`, admin approval (optionally two) for network, named viewers (by username or company email, pending until first sign-in); the last admin decision (declined, revoked, restricted) and its note shown to the owner | `different on purpose` — hosted non-goal: no private pages; enterprise default is only-me |
| Teams | none; one person per account | `team-<name>` namespaces, one role; members by username or company email (pending until first sign-in) | `different on purpose` — hosted accounts are single people (event participants get their own key) |
| Saved state | one JSON doc; atomic ops (`set`/`inc`/`append`/`remove`/`removeWhere`) + `PUT` with `If-Match`; 1 MB | one JSON doc; last-write-wins or versioned compare-and-set | `different on purpose` — hosted writes need a signed-in visitor or the owner's key; enterprise: every viewer is signed in and may write, keys included |
| Saved-state history and restore | none | last 20 writes kept, owner or team restores | `gap → hosted` — any signed-in visitor can overwrite state, so it needs the same undo |
| Collections and private collections | lists visitors append to; the owner deletes entries or empties a list (public ones too, 2026-09-27); private ones on a site's own origin, owner-only read, CSV export | none | `different on purpose` — enterprise decision 2026-09-23: per-site state is the only data store |
| Assets (runtime file uploads) | none | per-site uploads, served at `/_assets/{id}`, counted in quota | `gap → hosted` |
| Malware scan on upload | none | optional clamd, fail closed | `different on purpose` — clamd needs ~1 GB RAM, above the small-box floor |
| Upload validation | `internal/tarball` sanitize, size caps, `blockedExtensions` | same package lineage, same checks | `same` |
| Visitor sign-in on a site | Google or emailed code on the site's own host; host-only cookie + `X-SH-CSRF`; login-CSRF nonce | company OIDC session handed to the site host by a nonce-bound one-time code | `different on purpose` — public visitors vs company identity |
| Owner sign-in and sessions | emailed code or Google → API key kept in the browser; no owner cookie session | company OIDC only; revocable sessions, idle 30m / absolute 8h, sessions page; stored email refreshed from the IdP at each sign-in | `different on purpose` — enterprise constraint: the IdP is the only identity |
| Sign out and sign out everywhere | Sign out deletes the key the browser held (`POST /v1/me/sign-out`); "Sign out everywhere" (rotate) replaces every key and disconnects every connected app (2026-09-27) | Sign out revokes the session; sessions page "Sign out everywhere": every session, plus (default on) every API key and connected app, one audited transaction | `same` |
| Connected apps: list and disconnect | owner app list with Disconnect (`/v1/me/connections`) | `/auth/sessions` list with Disconnect (`/api/me/connections`, session only), audited | `same` |
| API keys: hashing | SHA-256, shown once (2026-09-26); `shk_` prefix (2026-09-27) | `shk_`, stored hashed | `same` |
| API keys: expiry | none; rotate replaces all | 90 days default, capped by `API_KEY_MAX_DAYS` | `gap → hosted` |
| API keys: scopes | none (every key is full) | `publish` (default) / `full` / `offboard`, deny-by-default | `gap → hosted` |
| API keys: list and revoke one | named keys (origin label or typed), last 4, last used; mint, list, revoke each from the owner app's Keys panel; any account key may mint (hosted has no browser session) (2026-09-27) | mint, list, revoke each; browser session only | `same` |
| Reissue another person's key | organiser/admin replaces a participant's keys with one new key (`POST /v1/admin/users/{id}/key`) | none (keys are self-service; admin disables the person) | `different on purpose` — hosted events hand out keys to people with no mailbox |
| MCP connector and OAuth | DCR, PKCE S256, rotating refresh, reuse revokes the grant, hourly sweep, tokens hashed | same design (hosted's adapter was ported from enterprise) | `same` |
| Connector token lifetime and reach | refresh 90 d sliding; Bearer also accepted on `/v1/*` | refresh 30 d from sign-in, TTLs capped; Bearer only on `/mcp` | `different on purpose` — hosted: sign in once and stay signed in, and hand-registered GPT Actions call REST |
| MCP tools | 27 tools, each a REST call | own set incl. teams, viewers, access, state history; each resolves to a route (tested) | `same` — tools follow each side's REST surface |
| MCP error hints | hint chosen by the error `code` (taken address, append-only list, reserved/invalid name, own address needed, suspended), HTTP status as fallback | hint per code and per tool family | `same` |
| Skills and plugin | `website-deploy`, `-builder`, `connect-domain`, `run-hackathon`; Claude + ChatGPT plugins; stale skill → `_notice` | `simple-host`, `simple-host-builder`, `fix-paths-for-subpath-hosting`; `plugin.zip`; obsolete skill → refused | `different on purpose` — each skill teaches its own product; a company can require current skills |
| Audit log | none; server log only | every mutation + visit (and every `restore`, `migrate-storage`, `reencrypt` run), hash-chained, `audit-verify`, SIEM stdout stream, export, daily retention that recovers rows stranded in the default partition | `different on purpose` — hosted decision 2026-09-05: nothing records an author; enterprise constraint: everything on record |
| Analytics | nginx/Caddy log → views, visitors, local geo; API metrics; `site_analytics` tool | in-app access log → daily views (bots split), downloads, counts only for owners | `different on purpose` — different serving paths; geo is a public-web need |
| Search and company showcase | none; sites are found by link or person page | full-text search of listed sites, `/showcase` | `different on purpose` — enterprise decision 2026-09-23: attribution is the discovery mechanism |
| Quotas | 100 sites per account; per-site archive cap `MAX_ARCHIVE_MB` | per-owner `QUOTA_MAX_SITES` + `QUOTA_MAX_BYTES` (stored bytes), usage on dashboard | `gap → hosted` — a per-owner stored-bytes cap protects a small box's disk |
| Rate limits and abuse caps | per-IP token buckets in memory; size caps; write auth; reserved names | per-pod buckets plus Postgres-shared counters for sign-in, hand-off, key mint, connector | `different on purpose` — one process needs no shared counters |
| Security headers and CSP | `SecurityHeaders`, nonce CSP on apex pages | `security.go`, same approach | `same` |
| Admin | admin key or admin user; usage, bulk participant accounts, delete account, give a participant a new key, API traffic; download every site as one archive | IdP admins; disable/enable (revokes sessions, keys, apps), offboard by email, access requests, rankings, export | `different on purpose` — event organiser vs company IT |
| Data export and account erasure | self-serve: "Download my data" (`GET /v1/me/export.tar.gz`: every site, account, keys' names, apps, visitor activity) and "Delete my account" (`DELETE /v1/me`, immediate and final, incl. the person's entries on others' sites; names retired); the admin's `DELETE /v1/admin/users/{id}` is the same erasure (2026-09-27) | admin only, for a disabled person: "Export data" (one zip of everything held) and "Delete person and all data" (sites skip Recently deleted; keys, apps, sessions, memberships, grants, own visits; old redirects stop; audit rows keep the id, some the name or email, until retention; name held; sign-in refused until an admin allows it again) | `different on purpose` — hosted people own their accounts (GDPR self-service); enterprise accounts belong to the company, whose IT handles data requests |
| Admin take-down and suspension | take a site down / put it back (`POST /v1/admin/sites/{id}/suspend`, `/restore`; a take-down page with the reason, nothing deleted, kept past the Recently-deleted window) and suspend / re-enable a person (`/v1/admin/users/{id}/suspend`, `/enable`); the owner sees the reason (2026-09-27) | restrict any site to only-me with a reason the owner sees; sticky (the owner cannot raise it, rename it or move it, `site_restricted_by_admin`); only an admin's Lift restores it; disable / enable a person; audited | `same` — both reversible and keep the evidence; enterprise keeps a restricted site open to its owner |
| Dashboard | `/dashboard` (sign-in; with a handle, a list linking to the owner app), owner app (sites: address, versions, rename, domain with DNS record and Check again, lists, saved data, Download, Delete, Recently deleted; Keys panel; Your data), per-site analytics page | `/dashboard`: keys, sites, access, viewers, assets, usage, Recently deleted | `same` — each shows its own features |
| AI create and voice input | Grok sidecar only, local speech-to-text | none | `different on purpose` — enterprise: the publisher is the person's own agent via MCP; content stays in the cluster |
| Event / hackathon instances | setup page, participant accounts, `simple-hack.app` names | none | `different on purpose` — this is the small-box edition's job |
| Storage backend | local disk (`DATA_DIR`), served by nginx or Caddy | S3-compatible bucket, pod cache, SSE, optional envelope encryption, retire sweep | `different on purpose` — one folder on one box vs replicas |
| Deployment model | one binary + Postgres + disk: systemd on simple-host.app; `deploy/install/install.sh` + Docker Compose + Caddy for a small box | Kubernetes (kustomize), cert-manager, least-privilege DB role, TLS-only DB and bucket, startup refusals, rollback-safe migrations | `different on purpose` — owner decision: hosted is the small-box edition, enterprise the cluster edition |
| Schema migrations and version stamp | `simple-host migrate` (tracked in `schema_migrations`, advisory lock, per-file transaction, `-status`, `-mark`) run by `install.sh` on every run; production applies by hand and marks; `simple-host version`, startup log, `version`/`commit`/`keep_versions` in admin usage | migrate init container with advisory lock and per-file transactions, `migrate -status`; `simple-host version`, `simplehost_build_info` | `same` |
| Health and metrics | `/healthz`, `/readyz` | `/healthz`, `/readyz`, `/metrics` on its own port; startup warnings (no admin, bucket versioning off) logged and exported | `different on purpose` — no metrics stack on a small box |
| Static, marketing and legal pages | landing, features, enterprise pages, terms, privacy, support | landing, docs, capabilities, install, changelog | `different on purpose` — public service vs internal install |
| Notifications | none (sign-in email only) | none (SIEM stream only) | `same` |

## Security fixes checked across

| Date | Fixed in | Fix | Other side |
|---|---|---|---|
| 2026-09-26 | hosted | API keys stored only as SHA-256 | enterprise already hashes keys: n/a |
| 2026-09-26 | hosted | visitor Google sign-in bound to the starting browser (login CSRF) | enterprise hand-off already nonce-bound: n/a |
| 2026-09-26 | enterprise | connector tokens accepted only on `/mcp` | hosted keeps Bearer on `/v1/*` on purpose (GPT Actions); see table |
| 2026-09-26 | hosted | a key (or connector/MCP token) writes saved state and collections only on sites its account owns, or as admin; before, any free account's key wrote every site | enterprise: a publish-scope key or MCP token writes the state and assets of every site its person may open (`company`/`listed`/`network` = the whole company) — `WriterAllowed` = `ViewerAllowed` (`internal/db/site_viewers.go`), no owner check for keys. By design under "anyone who may open the site may write it" (FEATURES/INTENT: every viewer is a signed-in company person, with a 20-write undo), not the same hole: no stranger gets a key. Gap worth a decision: that rule was written for people in a browser, and a CI or agent key reaching every company site's data is broader; not changed here |
| 2026-09-26 | hosted | site export took saved state by bare name, so an owner's `export.tar.gz` could carry another account's same-named site's state; now by site id. Also: the state/collection Origin check resolves the same site a keyed request targets, and the admin `allow-anonymous-writes` toggle takes `?owner=` | enterprise: every site lookup is owner-qualified (`username` + `name`), and its version export carries no saved data: n/a |
| 2026-09-27 | enterprise | people can disconnect a connected app and sign out everywhere themselves; stored email follows the IdP so offboarding by email matches | hosted already lists and disconnects apps; hosted Sign out now deletes the browser's key and Sign out everywhere (rotate) ends every key and app (2026-09-27, table) |
| 2026-09-27 | enterprise | page scripts' `esc()` escapes quotes (stored XSS through a pending viewer's email); grant emails strict ASCII; `/admin` confirms via data attributes | hosted's `esc()` already escapes `"` and `'` (admin, index, showcase, analytics pages); hosted has no grants by email: n/a |
| 2026-09-27 | enterprise | moved-site redirects only for people who can open the site; no hand-over to a person; restricted sites cannot be moved or renamed; deleted sites count toward quota | hosted has no transfer or moved-site redirects: n/a; hosted's Recently deleted already counts toward the site cap; a taken-down site cannot be deleted or renamed away |
| 2026-09-27 | enterprise | one lock order for concurrent changes (person/team row, then site names): a hand-over out of a team being deleted could leave the moved site's files queued for the sweep, and a bulk delete could delete a site handed on meanwhile; erasure deletes a person's old redirects so their held name sends no links to someone else's site | hosted serialises per account+site and has no teams, hand-over or moved-site redirects; account deletion takes every site's lock: n/a |
| 2026-09-27 | enterprise | `SECURITY DEFINER` database functions pinned `search_path = pg_catalog, public, pg_temp` with schema-qualified names (migration 0049; the app role could hijack them through a temporary table and run SQL as the owner); `PUBLIC` loses `TEMPORARY` and `CREATE` on `public`; an erased person can no longer sign straight back in; the access-log eraser refuses a live account | hosted has no `SECURITY DEFINER` functions, and its account deletion is the person's own, so signing up again is allowed on purpose: n/a |

## Section index

Every numbered `FEATURES.md` section, per repo, and the rows above that cover it.

| Repo | FEATURES.md section | Rows |
|---|---|---|
| hosted | Sites and deploy | Sites; Site rename; Recently deleted; Site export; Upload validation |
| hosted | Per-site and per-person addresses, and legacy redirects | Per-site hosts; Per-owner certificates; Small-box path model |
| hosted | Claimed `<name>.simple-host.app` and custom domains | Free names; Custom domains |
| hosted | Saved state (shared JSON per site) | Saved state; Saved-state history |
| hosted | Collections, including private collections | Collections and private collections |
| hosted | Visitor sign-in (Google, emailed code) | Visitor sign-in on a site |
| hosted | Owner auth (API keys, email codes, profile) | Owner sign-in; Sign out and sign out everywhere; API keys rows; Change a person's address; Data export and account erasure |
| hosted | MCP connector and OAuth (chat apps) | MCP connector; Connector token lifetime; Connected apps; MCP error hints |
| hosted | Skills and plugin distribution | Skills and plugin |
| hosted | Owner dashboard and owner app | Dashboard |
| hosted | Admin (operator) | Admin; Admin take-down and suspension; Data export and account erasure; Reissue another person's key |
| hosted | Analytics and geo | Analytics |
| hosted | Showcase / person index | Person / owner index page |
| hosted | AI create (Grok sidecar) and voice input | AI create and voice input |
| hosted | Hackathon / event instances (simple-hack.app) | Event / hackathon instances; Small-box path model |
| hosted | Enterprise and marketing pages | Static, marketing and legal pages |
| hosted | Legal and support pages | Static, marketing and legal pages |
| hosted | Abuse limits and hardening | Rate limits and abuse caps; Quotas; Security headers |
| hosted | Signals and notifications | Notifications |
| hosted | Operations (health, schema, CLI) | Health and metrics; Deployment model; Schema migrations and version stamp |
| hosted | MCP tool index (`internal/mcp/tools.go`, 27 tools) | MCP tools |
| hosted | Unplaced routes and tools | (index of FEATURES itself, no feature) |
| enterprise | Identity: OIDC sign-in, sessions, hand-off | Owner sign-in and sessions; Sign out and sign out everywhere; Visitor sign-in on a site |
| enterprise | API keys (CI and automation) | API keys rows |
| enterprise | MCP server, OAuth connector, plugin.zip | MCP connector; Connector token lifetime; Connected apps; MCP error hints; Skills and plugin |
| enterprise | Skills bundle and skill-version gate | Skills and plugin |
| enterprise | Sites: deploy, versions, rollback, delete | Sites; Recently deleted; Site rename; Hand a site to another owner; Site export; Per-site hosts; Quotas; Malware scan |
| enterprise | Bucket storage, cache, retire sweep, migrate-storage, restore and reencrypt | Storage backend |
| enterprise | Access levels and network approval | Who can open a site |
| enterprise | Named viewers (restricted sites) | Who can open a site |
| enterprise | Teams | Teams |
| enterprise | Saved state and its history | Saved state; Saved-state history |
| enterprise | Assets (uploaded files) | Assets |
| enterprise | Audit log, access log, export, retention | Audit log |
| enterprise | Admin page | Admin; Admin take-down and suspension; Data export and account erasure |
| enterprise | Dashboard | Dashboard |
| enterprise | Search, showcase, owner index | Search and company showcase; Person / owner index page |
| enterprise | Metrics, health, request log, rate limits | Health and metrics; Rate limits and abuse caps; Analytics |
| enterprise | Landing and static pages | Static, marketing and legal pages |
| enterprise | Configuration, database roles and startup refusals | Deployment model; Schema migrations and version stamp |
| enterprise | Owner hosts: per-owner certificates | Per-owner certificates; Per-site hosts |
