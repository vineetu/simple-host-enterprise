# Features

The one map to read before changing anything: every feature, the surfaces it
spans, and where each lives. Derived from the code, not older docs. Use it to
size the blast radius of a change before writing it.

**Update this file in the same commit as any feature change** — a route, an
MCP tool, a page, a table, a config variable. `scripts/check-features.sh`
(run by `make test` and CI) fails when a registered HTTP route pattern or an
MCP tool name is missing from this file.

Conventions. Routes are written exactly as registered on the mux
(`METHOD /pattern`). "Host-gate routes" are not mux patterns: `internal/handler/host_gate.go`
answers them itself on owner hosts (`<owner>.<base>`) and restricted-site hosts
(`<owner>--<site>.<base>`); the base host refuses them. MCP tools live in
`internal/mcp/tools.go` and each resolves to one of the REST routes below
(`go test ./internal/mcp/` asserts it). The skill is
`simple-host-plugin/skills/simple-host/` (`SKILL.md` plus `references/`).
Config names are documented in `docs/configuration.md`; schema in
`internal/migrate/sql/`.

---

## 1. Identity: OIDC sign-in, sessions, hand-off

- **What.** People sign in only through the company's OIDC provider; an
  account is created at first sign-in (username/email from claims). Admin is
  `ADMIN_EMAILS` or an OIDC claim. Revocable session rows, `__Host-` cookies
  signed with `SESSION_SIGNING_KEY`, idle and absolute limits
  (`SESSION_IDLE` 30m, `SESSION_TTL` 8h by default; capped at 8h and 24h,
  refused at startup beyond). A session on
  the base host is handed to an owner or restricted-site host by a one-time
  code (`/auth/handoff` on the base host mints; `/auth/session` on the target
  host redeems, nonce-bound against login CSRF). Every session cookie is bound
  to the host it was minted for; a hand-off cookie shares the sign-in's
  session row and expiry, so it never outlives it.
- **Status.** Built.
- **Routes.** `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout`,
  `GET /auth/sessions` (sessions page), `POST /auth/sessions/{id}/revoke`,
  `GET /auth/handoff`, `GET /api/me`.
  Host-gate: `GET /auth/session` (redeem, owner and restricted-site hosts).
- **MCP.** `get_account` (→ `GET /api/me`).
- **Skill.** `references/account-recovery.md` (Sign-in and API keys);
  `SKILL.md` §1–2.
- **Pages.** `/auth/sessions`; sign-in prompt on `/dashboard`.
- **Go.** `internal/handler/auth.go`, `handoff.go`, `user.go`, `origin.go`;
  `internal/auth/` (`middleware.go`, `session_cookie.go`, `hostsession.go`);
  `internal/oidc/`; `internal/db/identity.go`, `sessions.go`, `handoff.go`.
- **DB.** `users` (0001, 0017 email, 0023 OIDC identity), `sessions` (0021),
  `handoff_codes` (0024).
- **Config.** `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
  `OIDC_SCOPES`, `OIDC_USERNAME_CLAIM`, `OIDC_EMAIL_CLAIM`, `OIDC_HINT_DOMAIN`,
  `OIDC_ADMIN_CLAIM`, `OIDC_ADMIN_VALUE`, `ADMIN_EMAILS`,
  `ALLOWED_EMAIL_DOMAINS`, `SESSION_SIGNING_KEY`, `SESSION_TTL`,
  `SESSION_IDLE`, `PUBLIC_BASE_URL`, `RESERVED_LABELS`.

## 2. API keys (CI and automation)

- **What.** A signed-in person mints, lists and revokes their own keys
  (`shk_` prefix, stored hashed, 90 days by default, at most
  `API_KEY_MAX_DAYS`). Managing keys requires a browser session, never a key.
  Sent as `X-API-Key`. People and their agents use OIDC/MCP; keys are for CI.
  Each key has a scope chosen at mint, enforced in `auth.Middleware` against
  the matched route pattern (`internal/auth/scope.go`, deny-by-default, every
  route classified by a test): `publish` (default: deploy, update, rollback,
  list, versions, archives, saved data and assets including the host-gate
  site API, `GET /api/me`, `/mcp`), `full` (every non-admin route), `offboard`
  (admins only; `POST /api/admin/users/disable` and nothing else). Refusal is
  403 with a JSON `scope`. Existing keys became `full`.
- **Status.** Built.
- **Routes.** `GET /api/keys`, `POST /api/keys`, `DELETE /api/keys/{id}`.
- **MCP.** None (by design).
- **Skill.** `references/account-recovery.md` (Get a key; Revoked, lost, or
  extra keys).
- **Pages.** `/dashboard` "API keys" panel.
- **Go.** `internal/handler/keys.go`, `dashboard.go`; `internal/db/api_keys.go`;
  `internal/auth/middleware.go`, `scope.go`.
- **DB.** `api_keys` (0022, 0029 expiry, 0035 scope); `users.api_key`
  dropped (0025).
- **Config.** `API_KEY_MAX_DAYS`.

## 3. MCP server, OAuth connector, plugin.zip

- **What.** `/mcp` is a JSON-RPC MCP server whose tools call the REST routes
  in-process, carrying the caller's own identity. AI apps connect by OAuth:
  protected-resource metadata (RFC 9728), authorization-server metadata
  (RFC 8414), dynamic client registration (RFC 7591), PKCE authorize through
  the company OIDC sign-in and one consent screen, access tokens
  (`OAUTH_ACCESS_TTL`, 1h) and rotating refresh tokens whose lifetime
  (`OAUTH_REFRESH_TTL`, 30 days) runs from the sign-in that connected the app,
  not from the last rotation. The access token is accepted as
  `Authorization: Bearer` only on `/mcp` and the in-process calls its tools
  make (`ProtectMCP` marks the request context; `auth.Middleware` refuses an
  unmarked Bearer with 401). An hourly
  sweep deletes expired codes and tokens. `/plugin.zip` is an installable
  plugin (Claude plugin and Agent Plugins manifests plus the skills) already
  pointing at `<base>/mcp`.
- **Status.** Built.
- **Routes.** `POST /mcp`, `GET /mcp`, `DELETE /mcp`,
  `GET /.well-known/oauth-protected-resource`,
  `GET /.well-known/oauth-protected-resource/mcp`,
  `GET /.well-known/oauth-authorization-server`, `POST /oauth/register`,
  `GET /oauth/authorize`, `POST /oauth/authorize`, `POST /oauth/token`,
  `POST /oauth/revoke`, `GET /plugin.zip`.
- **MCP.** Every tool below, sections 1–11; the full list is in
  `internal/mcp/tools.go` `toolList()`.
- **Skill.** `SKILL.md` (Service); plugin manifests
  `simple-host-plugin/plugin.json`, `mcp.json`.
- **Pages.** `/oauth/authorize` consent page (rendered by `connector.go`).
- **Go.** `internal/mcp/` (`server.go`, `jsonrpc.go`, `tools.go`, `outputs.go`,
  `schemacheck.go`, `deploy.go`, `archive.go`); `internal/handler/connector.go`,
  `plugin_bundle.go`; `cmd/server/main.go` (mounts `/mcp`);
  `simple-host-plugin/embed.go`.
- **DB.** `oauth_clients`, `oauth_grants`, `oauth_codes`, `oauth_tokens` (0031).
- **Config.** `OAUTH_REDIRECT_HOSTS`, `OAUTH_ACCESS_TTL`, `OAUTH_REFRESH_TTL`,
  `PUBLIC_BASE_URL`.

## 4. Skills bundle and skill-version gate

- **What.** The skills in `simple-host-plugin/skills/` are served with
  `PUBLIC_BASE_URL` substituted in: a zip, a version manifest (with sha256 and
  minimum supported version), an immutable content-addressed zip, and Agent
  Skills discovery (`/.well-known/agent-skills/index.json` plus one
  `<skill>.zip` per allow-listed skill: `simple-host`, `simple-host-builder`,
  `fix-paths-for-subpath-hosting`; GET and HEAD, registered in a loop).
  `SkillVersionMiddleware` answers `skill_version_required` to a key-bearing
  client that sends no or an obsolete `X-Skill-Version` on management routes.
- **Status.** Built.
- **Routes.** `GET /skills.zip`, `GET /skills/version`,
  `GET /skills/sha256/{digest}/skills.zip`; discovery routes built from
  `agentSkillsDiscoveryRoot` in `ui.go` (not literal patterns).
- **MCP.** None.
- **Skill.** `SKILL.md` §1 (Check the skill version first);
  `references/updating.md`.
- **Pages.** `/install.html` (skill update URL), `/changelog.html` (release notes).
- **Go.** `internal/handler/ui.go`, `notice_middleware.go`, `client_kind.go`;
  `simple-host-plugin/`.
- **DB.** None. **Config.** `PUBLIC_BASE_URL`.

## 5. Sites: deploy, versions, rollback, delete

- **What.** A site is a tar.gz (or MCP file list) deployed into a namespace
  (a person or a team). Create/update are guarded by `If-Match` ETags;
  validation in `internal/tarball`. Each deploy is a new immutable version;
  five versions are kept (older ones retired to the bucket sweep). Rollback
  makes an earlier version live. Delete retires the whole site. Served at
  `<owner>.<base>/<site>/` (or the restricted host, section 8). The owner-scoped
  routes act on the caller's own namespace; the collaboration routes name the
  owner (person or team) explicitly.
- **Status.** Built.
- **Routes.** `POST /api/sites/{sitename}`, `PUT /api/sites/{sitename}`,
  `DELETE /api/sites/{sitename}`, `POST /api/sites/{sitename}/rollback`,
  `GET /api/sites/{sitename}/versions`, `GET /api/sites`,
  `GET /api/collaboration/sites`,
  `GET /api/collaboration/sites/{owner}/{sitename}`,
  `POST /api/collaboration/sites/{owner}/{sitename}`,
  `PUT /api/collaboration/sites/{owner}/{sitename}`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}`,
  `POST /api/collaboration/sites/{owner}/{sitename}/rollback`,
  `GET /api/collaboration/sites/{owner}/{sitename}/versions`,
  `GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/archive`.
  Host-gate: `GET /{site}/...` on the owner host (hosted content).
- **MCP.** `list_sites`, `get_site`, `deploy_site`, `list_site_versions`,
  `rollback_site`, `delete_site`, `list_site_files`, `read_site_file` (the last
  two read a version archive).
- **Skill.** `SKILL.md` §3, Canonical deployment workflow, Core collaboration
  and conflict rules; `references/packaging-and-validation.md`,
  `references/collaboration.md` §1–5 and §8, `references/frameworks.md`;
  `skills/fix-paths-for-subpath-hosting/`, `skills/simple-host-builder/`.
- **Pages.** `/dashboard` "Your sites".
- **Go.** `internal/handler/site.go`, `collaboration.go`, `site_mutation.go`,
  `serve.go`, `serve_self_traffic.go`, `host_gate.go`, `host.go`, `names.go`,
  `security.go`; `internal/tarball/`; `internal/db/queries.go`,
  `collaboration.go`.
- **DB.** `sites`, `versions` (0001; 0012 `versions.uploaded_by`), 0018 owner
  label uniqueness.
- **Config.** `PUBLIC_BASE_URL`, `RESERVED_LABELS`.

## 6. Bucket storage, cache, retire sweep, migrate-storage, restore and reencrypt

- **What.** The S3-compatible bucket is the site store:
  `sites/<id>/v<N>.tar.gz` per version and `sites/<id>/assets/<id>` per asset,
  immutable. The database decides which version is live; every replica serves
  through a bounded pod-local cache. Optional SSE and client-side AES-GCM
  envelope. Unreferenced objects are queued in `storage_retired` in the same
  transaction and deleted by a sweeper after a one-hour grace (every 5 min,
  `SKIP LOCKED`, safe on every replica). Operator subcommands:
  `simple-host migrate-storage` (one-time move off the old volume),
  `simple-host restore`, and `simple-host reencrypt` (rewrites every stored
  object under the first `BACKUP_ENVELOPE_KEY` in the key-bound form, so old
  keys can be removed and a plaintext install can adopt the envelope;
  idempotent, verified read-back, rewrites a version only once a committed
  row names it). A bucket fault does not fail `/readyz`
  (`simplehost_bucket_ok` instead).
- **Status.** Built.
- **Routes.** None of its own. **MCP.** None.
- **Skill.** None.
- **Go.** `internal/storage/` (`store.go`, `cache.go`, `sweep.go`,
  `objects.go`, `objects_s3.go`, `envelope.go`, `archive.go`, `keys.go`,
  `migrate.go`, `reencrypt.go`); `internal/db/storage.go`,
  `db.VersionExists`; `cmd/server/subcommands.go`.
- **DB.** `storage_retired` (0032).
- **Config.** `BACKUP_STORAGE_ENDPOINT`, `BACKUP_STORAGE_BUCKET`,
  `BACKUP_STORAGE_PREFIX`, `BACKUP_STORAGE_REGION`,
  `BACKUP_STORAGE_ACCESS_KEY_ID`, `BACKUP_STORAGE_SECRET_ACCESS_KEY`,
  `BACKUP_STORAGE_INSECURE_ALLOWED`, `BACKUP_SSE`, `BACKUP_SSE_KEY_ID`,
  `BACKUP_ENVELOPE_KEY` (or `BACKUP_ENVELOPE_KEY_FILE`),
  `BACKUP_ENVELOPE_PLAINTEXT_ALLOWED`, `CACHE_DIR`,
  `CACHE_MAX_BYTES`. See `docs/storage.md`.

## 7. Access levels and network approval

- **What.** `sites.access` is one of five levels: `only_me` (owner or team
  members; default), `specific` (plus named viewers, own host — section 8),
  `company` (anyone signed in with the link), `listed` (company plus showcase
  and search), `network` (anyone, no sign-in). `network` is a request with a
  reason (202) that an admin approves, declines or revokes on `/admin`;
  anonymous visitors to an approved site can read pages, assets and saved data
  and write nothing. `sites.public` mirrors `listed`/`network`.
- **Status.** Built.
- **Routes.** `POST /api/sites/{sitename}/access`,
  `POST /api/collaboration/sites/{owner}/{sitename}/access`,
  `POST /api/admin/access-requests/{owner}/{sitename}/approve`,
  `POST /api/admin/access-requests/{owner}/{sitename}/decline`,
  `POST /api/admin/access-requests/{owner}/{sitename}/revoke`.
- **MCP.** `set_site_access`.
- **Skill.** `references/collaboration.md` §6 (Who can open the site);
  `references/packaging-and-validation.md` (Who can open it);
  `simple-host-builder` §4.
- **Pages.** `/dashboard` access control per site; `/admin` "Access requests".
- **Go.** `internal/handler/access.go`, `host_gate.go`
  (`requireHostSessionOrNetwork`, `serveSiteAPI`); `internal/db/site_access.go`.
- **DB.** `sites.access`, `sites.network_requested_*` (0033); `sites.public`
  (0006).
- **Config.** None.

## 8. Named viewers (restricted sites)

- **What.** At level `specific` a site lists named viewers (people or teams).
  Granting the first viewer moves the site to `<owner>--<site>.<base>`, its own
  origin; only listed viewers (and the owner or team) can open it. Viewers
  read, never write. Removing the last viewer leaves the level, narrowing the
  site to its owner.
- **Status.** Built.
- **Routes.** `GET /api/collaboration/sites/{owner}/{sitename}/viewers`,
  `POST /api/collaboration/sites/{owner}/{sitename}/viewers`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}/viewers/{username}`,
  `GET /api/collaboration/sites/{owner}/{sitename}/viewer-candidates`.
  Host-gate: every path on the restricted-site host, including the nameless
  `/api/site/...` site API.
- **MCP.** `find_users`, `list_site_viewers`, `grant_site_viewer`,
  `revoke_site_viewer`.
- **Skill.** `references/collaboration.md` §7 (Named viewers).
- **Pages.** `/dashboard` viewers panel.
- **Go.** `internal/handler/viewers.go`, `host_gate.go`
  (`serveRestrictedSiteHost`), `host.go` (`SplitRestrictedSiteLabel`);
  `internal/db/site_viewers.go`.
- **DB.** `site_viewers` (0024).
- **Config.** None. See `docs/site-isolation.md`.

## 9. Teams

- **What.** A team is a namespace like a person, with one role: every member
  may do everything, including delete. Created by any person (capped per
  person). When the last active member leaves, the team and all its sites are
  deleted — refused with `409 confirm_team_delete` and `site_count` until the
  call carries `?confirm_name=<team>`; delete works the same way. A team whose
  members are all disabled is deleted by an admin. A team has no key and no
  sign-in.
- **Status.** Built.
- **Routes.** `POST /api/teams`, `GET /api/teams`,
  `GET /api/teams/{team}/members`, `GET /api/teams/{team}/member-candidates`,
  `POST /api/teams/{team}/members`,
  `DELETE /api/teams/{team}/members/{username}`,
  `POST /api/teams/{team}/leave`, `DELETE /api/teams/{team}`,
  `POST /api/admin/teams/{team}/delete`.
- **MCP.** `create_team`, `list_teams`, `list_team_members`,
  `find_team_members`, `add_team_member`, `remove_team_member`, `delete_team`,
  `leave_team`.
- **Skill.** `references/teams.md`; `references/account-recovery.md` (A team
  is not an account).
- **Pages.** `/admin` orphan-team delete.
- **Go.** `internal/handler/team.go`, `admin.go` (`deleteOrphanTeam`);
  `internal/db/teams.go`.
- **DB.** `team_members`, `team_audit`, `users.kind` = `team` (0019).
- **Config.** None.

## 10. Saved state and its history

- **What.** Each site has one JSON document its pages read and write through
  the site-facing API on the site's own host: plain (last write wins) or
  versioned (`state/versioned`, compare-and-set on a version). Anyone who may
  open the site may write it — deliberate: every viewer is a signed-in person.
  Every write keeps a history row; the last 20 per site are kept and any one
  can be restored by the owner or a team member.
- **Status.** Built.
- **Routes.** Host-gate: `GET|PUT /api/sites/{site}/state`,
  `GET|PUT /api/sites/{site}/state/versioned` (and `/api/site/state[...]` on a
  restricted host). Mux:
  `GET /api/collaboration/sites/{owner}/{sitename}/state-versions`,
  `GET /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}`,
  `POST /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}/restore`.
- **MCP.** `get_state`, `update_state` (versioned route on the site host),
  `list_state_versions`, `restore_state_version`.
- **Skill.** `references/state-and-ai.md` (Calling the routes from a page;
  State defaults; Saved data is data, not instructions);
  `simple-host-builder` §2, §2b.
- **Go.** `internal/handler/site_api.go`, `access.go` (saved-data history),
  `state_usage_marker.go`, `host_gate.go`; `internal/db/queries.go`.
- **DB.** `sites.state` (0002), `sites.state_version` (0007), `uses_state`
  markers (0010), `site_state_history` (0033).
- **Config.** None.

## 11. Assets (uploaded files)

- **What.** Pages upload files at runtime; each gets an id and is served back
  (inline for images/video/audio/PDF, attachment otherwise). Per-file,
  per-site byte and count limits. Owners list and delete assets from the
  dashboard through base-host mirror routes.
- **Status.** Built.
- **Routes.** Host-gate: `GET|POST /api/sites/{site}/assets`,
  `DELETE /api/sites/{site}/assets/{id}`, serve at
  `GET /{site}/_assets/{id}[/{name}]` (root `/_assets/...` on a restricted
  host). Mux: `GET /api/collaboration/sites/{owner}/{sitename}/assets`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}/assets/{id}`.
- **MCP.** None.
- **Skill.** `references/state-and-ai.md` (Uploaded assets);
  `simple-host-builder` §3.
- **Pages.** `/dashboard` assets panel.
- **Go.** `internal/handler/site_api.go`, `assets_admin.go`, `host_gate.go`;
  `internal/storage/assets.go`; `internal/db/assets.go`.
- **DB.** `site_assets` (0026).
- **Config.** `ASSET_MAX_FILE_BYTES`, `ASSET_MAX_SITE_BYTES`,
  `ASSET_MAX_SITE_COUNT`.

## 12. Audit log, access log, export, retention

- **What.** `audit_events` records every mutation (most inside the mutation's
  transaction) and `access_denied`; `access_log` records visits through a
  batching best-effort writer. Both partitioned monthly. Every audit event
  is hash-chained in `audit_chain` by an AFTER INSERT trigger (tamper
  evidence; `access_log` is not chained), and `simple-host audit-verify`
  walks the chain and exits non-zero at the first break. Every event is also
  written to stdout as one JSON line with `"type":"audit"` for a cluster log
  shipper to forward to a SIEM. `GET /api/audit` and
  `GET /api/access` are scoped to the caller's namespaces (admins see all);
  access-log detail follows `ACCESS_LOG_VISIBILITY`. Admins export either as
  CSV (formula-safe) or NDJSON. `simple-host prune` (a CronJob) drops
  partitions past retention, trims the chain's rows for them, and creates
  future ones.
- **Status.** Built.
- **Routes.** `GET /api/audit`, `GET /api/access`, `GET /api/admin/export`.
- **MCP.** None.
- **Skill.** None.
- **Pages.** `/dashboard` and `/admin` activity/visitor panels.
- **Go.** `internal/audit/` (`audit.go`, `db_recorder.go`, `access_writer.go`,
  `reader.go`, `prune.go`, `chain.go`); `internal/handler/audit_access.go`,
  `audit_helpers.go`, `admin_export.go`; `internal/db/audit.go`;
  `cmd/server/subcommands.go` (`prune`, `audit-verify`); `cmd/server/main.go`
  (the shared stdout JSON logger).
- **DB.** `audit_events`, `access_log` and their `_default` partitions (0027,
  0028, 0030); `audit_chain`, `audit_chain_head`, `audit_event_canonical()`,
  `audit_chain_append()` and trigger `audit_events_chain` (0036, owner-only).
- **Config.** `AUDIT_RETENTION_DAYS`, `ACCESS_LOG_RETENTION_DAYS`,
  `ACCESS_LOG_VISIBILITY`. The stream and the chain have no settings.

## 13. Admin page

- **What.** Server-rendered `/admin` for admins: users (disable/enable —
  disabling revokes sessions, API keys and connected apps in the same
  transaction as its audit row), orphan teams, access requests, rankings of users
  and sites (views, storage from a cached bucket measurement, updated), new
  users, state-backend usage, visitors and activity, all sites.
- **Status.** Built.
- **Routes.** `GET /admin`, `POST /api/admin/users/{username}/disable`,
  `POST /api/admin/users/{username}/enable`,
  `POST /api/admin/users/disable` (offboarding by `{"email"}`: every person
  account with that address, idempotent, 404 when none; an admin's session
  or an admin's `offboard` key); plus the admin routes in sections 7, 9, 12.
- **MCP.** None.
- **Pages.** `/admin`.
- **Go.** `internal/handler/admin.go`, `admin_rankings.go`,
  `admin_disk_usage.go`, `access.go` (`renderAccessRequests`).
- **DB.** `users.disabled_at` (0023), `site_daily_analytics` (0003, 0013).
- **Config.** `ADMIN_EMAILS`, `OIDC_ADMIN_CLAIM`, `OIDC_ADMIN_VALUE`. See
  INSTALL.md "Sessions and leavers".

## 14. Dashboard

- **What.** `/dashboard` on the base host: sign-in prompt when signed out;
  when signed in, API keys (mint/list/revoke), "Your sites" across the
  person's and their teams' namespaces with access level, viewers, assets,
  visitor counts, and a link to sessions. Calls the JSON routes of sections
  2, 5, 7, 8, 11 and 12 with the session cookie.
- **Status.** Built.
- **Routes.** `GET /dashboard`.
- **MCP.** None.
- **Skill.** `references/account-recovery.md` (Get a key).
- **Go.** `internal/handler/dashboard.go`.
- **DB.** Reads only. **Config.** `SESSION_IDLE`.

## 15. Search, showcase, owner index

- **What.** Search indexes the text of `listed`/`network` sites (a background
  worker re-extracts on every deploy, rollback and delete; PostgreSQL full
  text) and answers signed-in callers; clicks and impressions are recorded
  pseudonymously and pruned by a background loop. `/showcase` is the gallery
  of listed sites. The root of an owner host (`GET /` on `<owner>.<base>`) lists
  that owner's sites: everything to the owner, only listed sites to others,
  never a restricted one.
- **Status.** Built.
- **Routes.** `GET /api/search`, `POST /api/search/click`, `GET /showcase`.
  Host-gate: `GET /` on an owner host.
- **MCP.** None.
- **Skill.** `references/state-and-ai.md` (Public search).
- **Pages.** `/showcase`; owner-host index.
- **Go.** `internal/handler/search.go`, `showcase.go`, `owner_index.go`;
  `internal/search/` (`worker.go`, `extract.go`, `public.go`,
  `telemetry_pruner.go`); `internal/db/search.go`, `search_query.go`.
- **DB.** `site_search_documents`, `site_search_queue`,
  `site_search_index_status`, `site_search_queries`,
  `site_search_impressions`, `site_search_clicks` (0011).
- **Config.** None.

## 16. Metrics, health, request log, rate limits

- **What.** `/healthz` (liveness) and `/readyz` (database and schema; bucket is
  reported, not gating) on every host. `/metrics` on its own port, never on
  the Service or Ingress: request counts and latency, `simplehost_bucket_ok`,
  DB pool, build info. Structured request log. Process-wide rate limits and
  concurrency slots. Per-site daily views (bot traffic split out; deploy-check
  traffic via `CheckHeader` not counted as human) and file downloads.
- **Status.** Built.
- **Routes.** `GET /healthz`, `GET /readyz`, `GET /metrics` (metrics port).
- **MCP.** None.
- **Go.** `internal/handler/health.go`, `abuse_limits.go`, `client_kind.go`,
  `download_recorder.go`; `internal/metrics/`; `internal/reqlog/`;
  `internal/ratelimit/`.
- **DB.** `site_daily_analytics` (0003, 0013), `site_file_downloads` (0009).
- **Config.** `METRICS_PORT`, `PORT`, `HTTPS_REDIRECT_PORT`, `SECURE_MODE`,
  `TRUSTED_PROXY_CIDRS`.

## 17. Landing and static pages

- **What.** The base host serves embedded static pages and the API spec.
- **Routes.** `GET /` (file server: `index.html`, `docs.html`,
  `capabilities.html`, `install.html`, `changelog.html`, `openapi.yaml`,
  fonts, CSS).
- **Go.** `internal/handler/ui.go`, `internal/handler/static/`.
  `changelog.html` is the owner's to edit (see `CLAUDE.md`).

## 18. Configuration, database roles and startup refusals

- **What.** `internal/config` reads the environment; nothing that identifies
  an installation has a default. The process refuses to start on: any
  missing required value (all reported at once); a DSN whose `sslmode` is not
  `verify-full` with a root cert (unless `DB_INSECURE_ALLOWED`); a plain-http
  bucket endpoint (unless `BACKUP_STORAGE_INSECURE_ALLOWED`); a multi-tenant
  Entra ID issuer; a non-https `PUBLIC_BASE_URL` in secure mode;
  `OIDC_ADMIN_CLAIM`/`OIDC_ADMIN_VALUE` set alone; bucket keys set alone;
  `BACKUP_SSE_KEY_ID` without `BACKUP_SSE=aws:kms` (or the reverse); malformed
  `SESSION_SIGNING_KEY`, `BACKUP_ENVELOPE_KEY` or `TRUSTED_PROXY_CIDRS`;
  `API_KEY_MAX_DAYS` outside 1–365; `SESSION_TTL` over 24h, `SESSION_IDLE`
  over 8h or over `SESSION_TTL`, `OAUTH_ACCESS_TTL` over 24h or over
  `OAUTH_REFRESH_TTL`, `OAUTH_REFRESH_TTL` over 90 days; clashing ports; `DB_APP_PASSWORD` equal to
  `DB_PASSWORD`; `DB_APP_USER` combined with `DB_DSN`; a schema newer than the
  binary unless every newer migration is marked backward-compatible.
  `simple-host migrate` applies the schema and sets the least-privilege
  `simplehost_app` role's password; the server connects as that role.
- **Subcommands.** `migrate`, `restore`, `migrate-storage`, `prune`, `version`.
- **Go.** `internal/config/config.go`; `internal/migrate/`;
  `cmd/server/main.go`, `subcommands.go`.
- **DB.** `simplehost_app` grants (0020); every migration.
- **Config.** `DB_DSN` or `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`,
  `DB_PASSWORD`, `DB_SSLMODE`, `DB_SSL_ROOT_CERT`, `DB_APP_USER`,
  `DB_APP_PASSWORD`, `DB_INSECURE_ALLOWED`, `DB_INCLUSTER_EVALUATION`; all of
  the above. Full list: `docs/configuration.md`.

---

## Orphans and dormant surface

- Columns with no reader or writer left in the code: `sites.site_type`,
  `sites.site_type_version` and their index (0014). Kept one release after
  the classifier's removal so a rollback still finds them; drop them next.
- `reset_requests`, `key_reissues` and `ai_usage` are dropped by 0034.
- No MCP tool is orphaned: every tool resolves to a route above.
