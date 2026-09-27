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
answers them itself on owner hosts (`<owner>.<base>`: the owner's index page,
the fallback `/<site>/` addresses until the owner's certificate is ready, then
redirects of them), site hosts (`<site>.<owner>.<base>`, every site at every
access level, served from the root) and v1.2 `<owner>--<site>.<base>` hosts
(redirect only); the base host refuses them. MCP tools live in
`internal/mcp/tools.go` and each resolves to one of the REST routes below
(`go test ./internal/mcp/` asserts it). The skill is
`simple-host-plugin/skills/simple-host/` (`SKILL.md` plus `references/`).
Config names are documented in `docs/configuration.md`; schema in
`internal/migrate/sql/`.

---

## 1. Identity: OIDC sign-in, sessions, hand-off

- **What.** People sign in only through the company's OIDC provider; an
  account is created at first sign-in (username/email from claims; the
  username is not refreshed from later claims: an admin renames it,
  section 13). Admin is
  `ADMIN_EMAILS` or an OIDC claim. Revocable session rows, `__Host-` cookies
  signed with `SESSION_SIGNING_KEY`, idle and absolute limits
  (`SESSION_IDLE` 30m, `SESSION_TTL` 8h by default; capped at 8h and 24h,
  refused at startup beyond). A session on
  the base host is handed to an owner host or a site host by a one-time
  code (`/auth/handoff` on the base host mints; `/auth/session` on the target
  host redeems, nonce-bound against login CSRF); the first visit to each site
  host hands off transparently, one redirect round trip, no prompt. Every session cookie is bound
  to the host it was minted for; a hand-off cookie shares the sign-in's
  session row and expiry, so it never outlives it. Every sign-in (verified
  email, allowed domain) turns any pending viewer or team grants for that
  email (sections 8, 9) into real ones in the session's transaction, each
  audited as `pending_grant_converted`, but only when the verified `email`
  claim is plain ASCII and is the address the account now holds from a
  claim (exact, lower-cased match; not an address another account keeps). Every sign-in also refreshes
  the stored email from the verified claim (the account is found by
  subject); an address another person already holds is not taken over
  (`email_change` / `email_change_skipped` in the audit log). The sessions
  page lists sessions with Revoke, the person's connected apps with
  Disconnect (section 3), and "Sign out everywhere": every session, and by
  default every API key and connected app, revoked in one transaction
  (`sign_out_everywhere`), leaving the browser signed out. **No access and
  switch account.** A browser opening a site host whose site does not exist,
  or that the signed-in person may not open, gets the same 404 page: "This
  site doesn't exist or isn't shared with you", who they are signed in as
  (username and email), "Ask the person who sent you the link to share it
  with you", and Switch account. A signed-out browser is sent to sign in
  first in both cases, so the page confirms nothing; scripts and agents get
  the plain 404 (only a refused existing site is audited `access_denied`).
  The page is served with `Content-Security-Policy: sandbox` (an opaque
  origin, no script, `frame-ancestors 'none'`), so on the owner-path
  fallback another of the owner's sites cannot read who is signed in.
  Switch account (`GET /auth/switch` on the site or owner host, reserved
  like `/auth/session`) clears that host's cookie and goes to
  `/dashboard?switch=<site address>`, which offers "Sign out and switch";
  `POST /auth/logout` with form field `to` (an https address on this
  server's owner or site hosts, anything else ignored) signs out and returns
  there, which asks for sign-in again.
- **Status.** Built.
- **Routes.** `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout`,
  `GET /auth/sessions` (sessions page), `POST /auth/sessions/{id}/revoke`,
  `POST /auth/sessions/revoke-all` (sign out everywhere; form field
  `credentials` set = keys and apps too), `GET /auth/handoff`, `GET /api/me`.
  Host-gate: `GET /auth/session` (redeem, owner and site hosts),
  `GET /auth/switch` (switch account, owner and site hosts).
- **MCP.** `get_account` (→ `GET /api/me`).
- **Skill.** `references/account-recovery.md` (Sign-in and API keys);
  `SKILL.md` §1–2.
- **Pages.** `/auth/sessions` (sessions, connected apps, sign out
  everywhere); sign-in prompt on `/dashboard`.
- **Go.** `internal/handler/auth.go`, `handoff.go`, `user.go`, `origin.go`, `no_access.go`;
  `internal/auth/` (`middleware.go`, `session_cookie.go`, `hostsession.go`);
  `internal/oidc/`; `internal/db/identity.go`, `sessions.go`, `handoff.go`.
- **DB.** `users` (0001, 0017 email, 0023 OIDC identity), `sessions` (0021),
  `handoff_codes` (0024); converts `pending_site_viewers`,
  `pending_team_members` (0045).
- **Config.** `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
  `OIDC_SCOPES`, `OIDC_USERNAME_CLAIM`, `OIDC_EMAIL_CLAIM`, `OIDC_HINT_DOMAIN`,
  `OIDC_ADMIN_CLAIM`, `OIDC_ADMIN_VALUE`, `ADMIN_EMAILS`,
  `ALLOWED_EMAIL_DOMAINS`, `SESSION_SIGNING_KEY`, `SESSION_TTL`,
  `SESSION_IDLE`, `PUBLIC_BASE_URL`, `RESERVED_LABELS`.

## 2. API keys (CI and automation)

- **What.** A signed-in person mints, lists and revokes their own keys
  (`shk_` prefix, stored hashed, `API_KEY_DEFAULT_DAYS` (90) by default, at most
  `API_KEY_MAX_DAYS`). Managing keys requires a browser session, never a key.
  Sent as `X-API-Key`. People and their agents use OIDC/MCP; keys are for CI.
  Each key has a scope chosen at mint, enforced in `auth.Middleware` against
  the matched route pattern (`internal/auth/scope.go`, deny-by-default, every
  route classified by a test): `publish` (default: deploy, update, rollback,
  list, versions, archives, saved data and assets including the host-gate
  site API, `GET /api/me`, `/mcp`), `full` (every non-admin route), `offboard`
  (admins only; `POST /api/admin/users/disable` and nothing else). Refusal is
  403 with a JSON `scope`. Existing keys became `full`. A minted key and
  its paste-back block stay on the dashboard until dismissed (the list
  gains the row in place; nothing reloads). Each key keeps its own last four
  characters (`last4`, shown as "ends …abcd"; keys minted before 0050 show
  "earlier key"), and the dashboard row shows last used and "expires soon"
  inside `API_KEY_EXPIRY_WARNING_DAYS` (14). A refused key says why in a 401 `code`: `key_expired`
  (with the date and "mint a new one on the dashboard"), `key_revoked` or
  `key_not_recognised` (also for any key of a disabled person, so a found
  key does not reveal that its owner left). While a key has
  `API_KEY_EXPIRY_WARNING_DAYS` or less left, every response to it carries `X-Key-Expires` (RFC 3339) and an
  `X-Simple-Host-Notice` line. A CI job deploys with one call: `?create=true` on
  `PUT /api/sites/{sitename}` or `PUT /api/collaboration/sites/{owner}/{sitename}`
  creates a missing site (the create route's rules, `201`) and updates an
  existing one; without it a missing site stays `404`. A create of a name
  that exists answers `409 site_exists` naming that call (`docs/ci.md`). An admin revokes a leaked key by pasting it on
  /admin (`POST /api/admin/keys/revoke`, section 13).
- **Status.** Built.
- **Routes.** `GET /api/keys`, `POST /api/keys`, `DELETE /api/keys/{id}`.
- **MCP.** None (by design).
- **Skill.** `references/account-recovery.md` (Get a key; Key scopes;
  Revoked, lost, or extra keys).
- **Pages.** `/dashboard` "API keys" panel.
- **Go.** `internal/handler/keys.go`, `dashboard.go`; `internal/db/api_keys.go`;
  `internal/auth/middleware.go`, `scope.go`.
- **DB.** `api_keys` (0022, 0029 expiry, 0035 scope, 0050 `last4`);
  `users.api_key` dropped (0025).
- **Config.** `API_KEY_MAX_DAYS`, `API_KEY_DEFAULT_DAYS`, `API_KEY_EXPIRY_WARNING_DAYS`.

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
  sweep deletes expired codes and tokens. A person sees their connected
  apps (name, connected, last used, and the browser that pressed Allow as
  a summary such as "Chrome on macOS", so two connections of the same app
  can be told apart; `device` in `GET /api/me/connections`; never an IP
  address or the raw header; only grants with a live token) and
  disconnects one (the grant and its tokens deleted, audited
  `connector_revoke`) on `/auth/sessions`, through two session-only routes.
  `/plugin.zip` is an installable
  plugin (Claude plugin and Agent Plugins manifests plus the skills) already
  pointing at `<base>/mcp`.
- **Status.** Built.
- **Routes.** `POST /mcp`, `GET /mcp`, `DELETE /mcp`,
  `GET /.well-known/oauth-protected-resource`,
  `GET /.well-known/oauth-protected-resource/mcp`,
  `GET /.well-known/oauth-authorization-server`, `POST /oauth/register`,
  `GET /oauth/authorize`, `POST /oauth/authorize`, `POST /oauth/token`,
  `POST /oauth/revoke`, `GET /plugin.zip`, `GET /api/me/connections`,
  `DELETE /api/me/connections/{id}` (both browser session only).
- **MCP.** Every tool below, sections 1–12; the full list is in
  `internal/mcp/tools.go` `toolList()`.
- **Skill.** `SKILL.md` (Service); plugin manifests
  `simple-host-plugin/plugin.json`, `mcp.json`.
- **Pages.** `/oauth/authorize` consent page (rendered by `connector.go`);
  "Connected apps" on `/auth/sessions` (`auth.go`, routes in `keys.go`).
- **Go.** `internal/mcp/` (`server.go`, `jsonrpc.go`, `tools.go`, `outputs.go`,
  `schemacheck.go`, `deploy.go`, `archive.go`); `internal/handler/connector.go`,
  `plugin_bundle.go`; `cmd/server/main.go` (mounts `/mcp`);
  `simple-host-plugin/embed.go`.
- **DB.** `oauth_clients`, `oauth_grants`, `oauth_codes`, `oauth_tokens` (0031);
  `oauth_codes.device_hint` / `oauth_grants.device_hint` (0055, the summary
  from `deviceHint` in `internal/handler/device_hint.go`, copied from the
  code to the grant on redemption).
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
  `QUOTA_MAX_VERSIONS` (5) are kept, the oldest pruned in the deploy's own
  transaction (never the live one) and retired to the bucket sweep. Every
  deploy is checked against the owner's quota (sites, stored bytes) under a
  per-owner lock (409 `site_limit`, 413 `storage_quota`), and, with
  `CLAMD_ADDR` set, every file is scanned before anything is stored (422
  `malware_found`, 503 `scanner_unavailable`, fail closed). Rollback
  makes an earlier version live. Delete is recoverable for
  `DELETED_RETENTION_DAYS` (30; `db.DeletedSiteRetention`; the purge date is stored
  in `sites.purge_at` at deletion, so a later change applies to new deletions): the row is marked `deleted_at` and stops
  serving and listing at once, while its versions, objects, saved data and
  history, access level, viewers and asset records stay; its name stays
  held (409 `name_held`). The owner or a team member lists them
  (`GET /api/deleted-sites`, dashboard "Recently deleted") and restores one
  whole (`site_restore`; 409 `name_taken` if a live site of the owner now
  holds its name or address). A recently deleted site keeps counting toward
  its owner's site and storage quota until its window ends, so the
  restore's quota re-check only refuses an owner already over; past the
  window a restore is 404 even before the sweeper runs. Idle-site cleanup
  (opt-in, `IDLE_CLEANUP_DAYS`, off by default; `internal/handler/idle_cleanup.go`,
  hourly on every replica, atomic per site): a site nobody has opened
  (owner and team included; bots and previews not), deployed to (held
  deploys too), or read or written saved data on for that many days
  (`sites.last_used_at`, 0053: bumped at most hourly per site by `serve.go`,
  the site API and deploys, `site_use.go`; sites older than 0053 start at
  the migration time, so the cleanup waits a full period rather than
  judging on missing data) is marked
  (`sites.idle_since`; audited `site_idle_marked` as `system`), its owner or
  every team member sees "Not used lately" on the dashboard (the date it
  moves, Keep, and Download: the whole-site zip of section 5's download
  link) and, with `SMTP_URL` (STARTTLS required unless `?insecure=1`;
  `SMTP_FROM` may carry a display name; non-ASCII subjects RFC 2047
  encoded), gets an
  email ("not opened or changed in N days"); admins see the list on /admin.
  The final check and the delete run under the site's lock and row lock, so
  a use or Keep in flight either lands first (and saves the site) or finds
  it gone. A visit, deploy, saved-data read or write, or restore
  unmarks it (`site_idle_cleared`); Keep (`POST .../keep`, `{"keep": false}`
  undoes it; `sites.idle_keep`; `site_idle_keep`) takes it out for good.
  `IDLE_CLEANUP_GRACE_DAYS` (30) after marking (`db.IdleGrace`), still unused and not kept, it
  moves to Recently deleted like an owner's delete (`site_delete`, `system`,
  `reason` `idle`). An admin restores any from `/admin`, and an
  admin's "Delete sites" for a leaver lands there too. After the window the
  sweeper purges the row and retires the objects (section 6). A team's
  deletion still removes its sites for good. Every site, at
  every access level, is served at the root of its own host
  `<site>.<owner>.<base>/` (e.g. `todo.alice.<base>/`) once the owner's
  `*.<owner>.<base>` certificate is ready (section 19); until then it is
  served at the fallback `<owner>.<base>/<site>/`, where the owner host
  serves content, the named site API, `_assets` and hand-off itself. Every
  response carries `url` in whichever form is current; agents quote `url`
  from the latest response and never compose it. A site named before v1.3
  that is not a DNS label keeps its name; its address uses the folded name
  plus `-` and 6 hex digits. New names: lowercase letters, digits and
  hyphens, starting and ending with a letter or digit, at most 63
  characters, not starting `xn--` (400 `invalid_site_name`); a new name
  equal to the address of an older site of the same owner is 409
  `name_conflict`. Once the owner is ready, `<owner>.<base>/<site>/...` and
  `<owner>.<base>/api/sites/<site>/...` redirect to the site host (301
  GET/HEAD, 308 otherwise, path and query kept), computed from the address
  alone, so a redirect confirms nothing. v1.2 `<owner>--<site>.<base>` hosts
  redirect the same way to the site's current address. The owner-scoped
  routes act on the caller's own namespace; the collaboration routes name the
  owner (person or team) explicitly, by its stored name (`team-sales`).
  **Move to a team and rename.** The owner can move their site into a team
  they are in, and a member can move a team's site into another team they
  are also in (`transfer`, `{"to"}`; `sales` and `team-sales` both name the
  team). Nobody hands a site to a person: only an admin's leaver flow does
  (section 13). Or the site gets a new name (`rename`, `{"name"}`, the
  new-site name rules). Only the row's owner or name changes: versions,
  saved data and its history, assets, access level and named viewers
  (pending ones too) are keyed by the site id and stay, except that a
  transfer drops network access (an approved site goes to `company`, a
  pending request is withdrawn; audited `network_access_reverted`) and a
  rename keeps it. Membership of both teams is re-checked, under lock,
  inside the move's transaction. A recently deleted site cannot be moved or
  renamed (404 until restored), nor can a site an admin restricted (409
  `site_restricted_by_admin` with the reason). The old address
  `<old part>.<old owner>.<base>` is kept in `site_redirects` and redirects
  (301 GET/HEAD, 308 otherwise, path and query kept; a named site-API path
  becomes the nameless `/api/site/...`) to the current address, following
  later moves, until a live site takes that address (live always wins),
  but only for a caller who could open the site there: anyone while it is
  open to the network; otherwise a caller with a host session on the old
  address or a key, whom the site's access rule admits (a navigation with
  no session is taken through the hand-off on the old host first). Anyone
  else gets the ordinary not-found, never a Location. The fallback owner
  path gates the same way; a v1.2 host sends a moved address to its current
  shape on the old owner, never to the new name. While the site it points
  to is recently deleted, the old address answers the ordinary not-found.
  Refused: `404 destination_not_found` (anything but a team the caller is
  in: no such team, a person, an inactive account; one message, whatever
  the reason), `409 name_conflict` (the team has a site with that name or
  address), `409 name_held` (a recently deleted site of the team holds it),
  `409 site_limit` / `413 storage_quota` (the team's quota), `400
  same_owner` / `same_name`. Full-scope key or session. Audited as
  `site_transfer` / `site_rename` with `from`, `to`, `from_name`, `name`,
  `from_owner_id`, `to_owner_id`; a transfer is recorded twice, once in each
  namespace (owner = the receiver, and owner = the previous owner), so both
  audit logs show it; the search entry is re-indexed. An admin moves or
  deletes the sites of a disabled person or of a team with no active member
  (section 13); nobody else's. Visitor counts are kept per address, so a
  moved site's Visitors tab starts again.
  **Preview before live.** An update sent with `?publish=false` (MCP
  `deploy_site` `publish: false`) stores the new version, quota-checked and
  scanned like any deploy, without making it live: `active_version` and the
  ETag stay, the answer carries `new_version`, and it is audited as
  `site_update` with `published: false`. A create refuses it (400
  `publish_required`). Every version list carries `live`. The owner or a
  team member mints a preview link for any kept version (`GET
  .../versions/{version}/preview`, MCP `preview_version`, "Preview" in the
  Manage panel's Versions list): the site's own address plus
  `_preview/<version>-<expiry>-<HMAC>/`, signed with the session signing key
  over site id, version and expiry, valid `PREVIEW_LINK_TTL` (1h). The host gate serves it
  (on the site host, and on the fallback owner path) only to a host session
  of the owner or a team member (`db.PreviewAllowed`, whatever the access
  level; anyone else 404, audited `access_denied` `not_owner_preview`;
  signed out, the hand-off), `X-Robots-Tag: noindex`, `Cache-Control:
  no-store`, not counted as a visit. An expired link is 410; a signature
  that does not verify is ordinary content. A save or upload from a preview
  page is 403 `preview_read_only` when the browser reports the preview page
  (Referer under a verifying preview path). Limitation: a previewed page
  that suppresses its Referer (`<meta name="referrer" content="no-referrer">`
  or `origin`, or `fetch` with `referrerPolicy: 'no-referrer'`) is not
  recognised, and its saves reach the live site's data; only the owner and
  team can open a preview, so this is not a way in for anyone else. Make live is the rollback to that version ("Make
  live" in Versions).

  **Download a site.** The owner or a team member gets a download address
  (`POST .../export-link`, full-scope key or session; `{url, expires_at}`)
  that works once, within `EXPORT_LINK_TTL` (10m), with no key or cookie (`GET
  /api/site-export/{token}`; the first download to start uses it up,
  `site_export_links_used`, and a second answers 410; HEAD answers headers
  only, without building the zip, auditing or using the link; the request
  log records the path as `/api/site-export/[redacted]`, and a preview
  link's token is redacted the same way in the request and access logs): one zip of `site.json`, `saved-data.json`,
  `saved-data-history.json`, `versions.json`, `assets.json`, the live
  version's files under `files/` and each uploaded file under
  `assets/<id>`, written by the same code as the admin's person export
  (`writeSiteExport`). The token is HMAC-signed with the session signing
  keys under its own domain string (a session cookie never verifies as one)
  and names the site id and the caller; at download the caller must still
  be active and still own the site or belong to its team, and the site must
  be the same one (not deleted, not re-created under the name), else 404.
  Audited `site_export` when downloaded. **Shared with me.** `GET
  /api/collaboration/sites?include=shared` appends the sites the caller, or
  a team they are in, is a named viewer of (not their own or their teams',
  not while the site is `only_me`) as entries with `access_role: "viewer"`
  and `shared_via` (the team's name, or empty for a grant naming the caller),
  without analytics, network request or admin decision. It is a listing
  only: no management route resolves a viewer. MCP `list_sites` always asks
  for them.
- **Status.** Built.
- **Routes.** `POST /api/sites/{sitename}`, `PUT /api/sites/{sitename}`,
  `DELETE /api/sites/{sitename}`, `POST /api/sites/{sitename}/rollback`,
  `GET /api/deleted-sites`, `POST /api/sites/{sitename}/restore`,
  `POST /api/collaboration/sites/{owner}/{sitename}/restore`,
  `GET /api/admin/deleted-sites`,
  `POST /api/admin/deleted-sites/{owner}/{sitename}/restore`,
  `GET /api/sites/{sitename}/versions`, `GET /api/sites`,
  `GET /api/collaboration/sites`,
  `GET /api/collaboration/sites/{owner}/{sitename}`,
  `POST /api/collaboration/sites/{owner}/{sitename}`,
  `PUT /api/collaboration/sites/{owner}/{sitename}`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}`,
  `POST /api/collaboration/sites/{owner}/{sitename}/rollback`,
  `GET /api/collaboration/sites/{owner}/{sitename}/versions`,
  `GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/archive`,
  `GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/preview`,
  `POST /api/sites/{sitename}/transfer`,
  `POST /api/collaboration/sites/{owner}/{sitename}/transfer`,
  `POST /api/sites/{sitename}/rename`,
  `POST /api/collaboration/sites/{owner}/{sitename}/rename`,
  `POST /api/sites/{sitename}/keep`,
  `POST /api/collaboration/sites/{owner}/{sitename}/keep`,
  `POST /api/collaboration/sites/{owner}/{sitename}/export-link`,
  `GET /api/site-export/{token}`.
  Host-gate: every path on the site host `<site>.<owner>.<base>` (hosted
  content, root-served); `/{site}/...` on the owner host (served before the
  owner is ready, redirected after); every path on a v1.2
  `<owner>--<site>.<base>` host (redirect); `_preview/<token>/...` under
  either content path (preview).
- **MCP.** `list_sites`, `get_site`, `deploy_site`, `list_site_versions`,
  `preview_version`,
  `rollback_site`, `delete_site`, `list_deleted_sites`, `restore_site`,
  `list_site_files`, `read_site_file` (the last
  two read a version archive), `transfer_site`, `rename_site`, `keep_site`,
  `export_site` (returns the download address).
- **Skill.** `SKILL.md` §3, Canonical deployment workflow, Core collaboration
  and conflict rules; `references/packaging-and-validation.md`,
  `references/collaboration.md` §1–5 and §8, `references/frameworks.md`;
  `skills/fix-paths-for-subpath-hosting/`, `skills/simple-host-builder/`.
- **Pages.** `/dashboard` "Your sites" (each links to its address with its live version and last update; Manage: Versions with
  Preview and Make live, Download site, Rename, Move to a team, Delete with the site name typed back; Rename and Move are off while an admin's
  restriction stands), "Shared with me", "Recently deleted" and "Not used lately" (idle cleanup); `/admin` "Recently
  deleted", "Not used lately".
- **Go.** `internal/handler/site.go`, `site_restore.go`, `collaboration.go`, `site_mutation.go`, `preview.go`, `dashboard.go`,
  `site_move.go`, `admin_move.go`, `site_export.go`, `admin_erase.go` (`writeSiteExport`), `idle_cleanup.go`, `site_use.go`, `smtp_mailer.go`,
  `upload_limits.go`, `serve.go`, `serve_self_traffic.go`, `host_gate.go`, `host.go`, `names.go`,
  `security.go`; `internal/tarball/`; `internal/scan/clamd.go`;
  `internal/db/queries.go`, `collaboration.go` (`ListSharedSites`), `quota.go`, `site_move.go`, `deleted_sites.go`,
  `erase.go` (`SiteExportQueries`), `idle.go`; `internal/config/idle.go`.
- **DB.** `sites`, `versions` (0001; 0012 `versions.uploaded_by`; 0037
  `versions.size_bytes`), 0018 owner label uniqueness, `site_redirects`
  (0043, backward-compatible), 0044 `sites.deleted_at`/`deleted_by`,
  0051 `sites.idle_since`/`idle_keep`, 0052 `site_viewers_principal_idx`
  (Shared with me), 0053 `sites.last_used_at` and `site_export_links_used`,
  0054 `sites.purge_at`/`idle_delete_at` (dates given at deletion and idle
  mark) (all backward-compatible).
- **Config.** `PUBLIC_BASE_URL`, `RESERVED_LABELS`, `QUOTA_MAX_SITES`,
  `QUOTA_MAX_BYTES`, `QUOTA_MAX_VERSIONS`, `CLAMD_ADDR`, `CLAMD_TIMEOUT`,
  `IDLE_CLEANUP_DAYS`, `SMTP_URL`, `SMTP_FROM`, `DELETED_RETENTION_DAYS`,
  `IDLE_CLEANUP_GRACE_DAYS`, `IDLE_CLEANUP_MAX_EMAILS`, `PREVIEW_LINK_TTL`,
  `EXPORT_LINK_TTL`, `MAX_ARCHIVE_BYTES`, `MAX_FILES_PER_SITE`,
  `UPLOAD_CONCURRENCY`.

## 6. Bucket storage, cache, retire sweep, migrate-storage, restore and reencrypt

- **What.** The S3-compatible bucket is the site store:
  `sites/<id>/v<N>.tar.gz` per version and `sites/<id>/assets/<id>` per asset,
  immutable. The database decides which version is live; every replica serves
  through a bounded pod-local cache. Optional SSE and client-side AES-GCM
  envelope. Unreferenced objects are queued in `storage_retired` in the same
  transaction and deleted by a sweeper after a one-hour grace (every 5 min,
  `SKIP LOCKED`, safe on every replica). The same loop records the stored
  size of any version that has none (`versions.size_bytes`, 0037) from the
  bucket, for the owner quota, and first purges every deleted site past its
  30-day recovery window (row removed, `sites/<id>/` queued for retirement).
  Operator subcommands:
  `simple-host migrate-storage` (one-time move off the old volume),
  `simple-host restore` (into a name a recently deleted site holds, it
  undeletes that row, bringing its saved data, viewers and assets back), and `simple-host reencrypt` (rewrites every stored
  object under the first `BACKUP_ENVELOPE_KEY` in the key-bound form, so old
  keys can be removed and a plaintext install can adopt the envelope;
  idempotent, verified read-back, rewrites a version only once a committed
  row names it). `restore`, `migrate-storage` and `reencrypt` each record
  one `system` audit event per run (`site_restore`, `storage_migrate`,
  `storage_reencrypt`, with counts; not for `-dry-run`); a failed audit
  write makes the command exit non-zero. `simple-host verify-storage`
  (read-only) lists every live version and live uploaded file of sites in
  use and in Recently deleted that is missing from the bucket, by object
  key, and exits non-zero when any is: the check after a database
  point-in-time restore. The restore drill (`docs/install.md`, "Restore
  drill") is tested end to end (`cmd/server/restore_drill_db_test.go`:
  publish, delete, `restore`, serves; purge, `restore`, serves;
  `verify-storage` names a lost object) and run by `make smoke`. A bucket
  fault does not fail `/readyz` (`simplehost_bucket_ok` instead). Each site
  also has `sites/<id>/manifest.json` (owner label and kind, a person's
  sign-in identity as a SHA-256 of issuer and subject or a team's id, never
  an email; site name, live version, deleted and admin-restricted state,
  its uploaded files' names, types, sizes and digests; a write number),
  rewritten best effort after every deploy, rollback, rename, hand-over,
  delete, restore, restriction, file upload and file delete, and by
  `restore`, one write at a time per site (an advisory lock) from committed
  state; deleted at once when a site is purged or its person erased
  (`internal/storage/manifest.go`, `internal/handler/site_manifest.go`);
  `reencrypt` rewrites it through the same path, never from stale bytes.
  `simple-host rebuild-index` (read-only; `-apply` to act) lists every
  site the bucket alone can bring back after the database is lost, and
  recreates, under the same site id, each whose owner is found: a person
  by sign-in identity (never by username), a team by id or an explicit
  `-map <owner>=<account>`; after validating every field as a create would
  (refused and listed otherwise), only with the manifest's own live version
  (a site whose live archive is missing is refused, never given a newer
  one), deleted sites back in Recently deleted and restricted ones
  restricted; `-apply` refuses a database that has sites unless
  `-force-live-db`. Site row, every kept version, and uploaded files;
  audited `site_restore` with `from: bucket_rebuild`.
  Saved data, access, viewers and team members are database-only and are
  not recovered (`docs/storage.md`, "Rebuilding from the bucket alone";
  `cmd/server/rebuild_index_db_test.go`).
- **Status.** Built.
- **Routes.** None of its own. **MCP.** None.
- **Skill.** None.
- **Go.** `internal/storage/` (`store.go`, `cache.go`, `sweep.go`,
  `objects.go`, `objects_s3.go`, `envelope.go`, `archive.go`, `keys.go`,
  `migrate.go`, `reencrypt.go`, `sizes.go`, `manifest.go`); `internal/db/storage.go`,
  `quota.go`, `storage_check.go`, `site_manifest.go`, `db.VersionExists`;
  `internal/handler/site_manifest.go`;
  `cmd/server/subcommands.go`, `verify_storage.go`, `rebuild_index.go`.
- **DB.** `storage_retired` (0032), `versions.size_bytes` (0037).
- **Config.** `BACKUP_STORAGE_ENDPOINT`, `BACKUP_STORAGE_BUCKET`,
  `BACKUP_STORAGE_PREFIX`, `BACKUP_STORAGE_REGION`,
  `BACKUP_STORAGE_ACCESS_KEY_ID`, `BACKUP_STORAGE_SECRET_ACCESS_KEY`,
  `BACKUP_STORAGE_INSECURE_ALLOWED`, `BACKUP_SSE`, `BACKUP_SSE_KEY_ID`,
  `BACKUP_ENVELOPE_KEY` (or `BACKUP_ENVELOPE_KEY_FILE`),
  `BACKUP_ENVELOPE_PLAINTEXT_ALLOWED`, `CACHE_DIR`,
  `CACHE_MAX_BYTES`. See `docs/storage.md`.

## 7. Access levels and network approval

- **What.** `sites.access` is one of five levels: `only_me` (owner or team
  members; default), `specific` (plus named viewers — section 8),
  `company` (anyone signed in with the link), `listed` (company plus showcase
  and search), `network` (anyone, no sign-in). `network` is a request with a
  reason (202) that an admin approves, declines or revokes on `/admin`;
  anonymous visitors to an approved site can read pages, assets and saved data
  on its own host and write nothing. `sites.public` mirrors `listed`/`network`. The requester
  (for a team site, the member who asked) can never approve their own
  request. With `NETWORK_ACCESS_APPROVALS=2` two different admins must
  approve: the first is recorded and audited as
  `network_access_approval_added` ("1 of 2 approvals" on `/admin`,
  `network_request.approvals`/`approvals_required` for the owner), the second
  opens the site (`network_access_approved`) in the same locked transaction;
  a decline, a level change or a new request clears partial approvals.
  The last admin decision is kept on the site for its owner
  (`access_decision`: `declined` or `revoked`, with when and the admin's
  optional note, until the next network request) and shown on the
  dashboard and by `get_site`/`list_sites`. An admin can also restrict any
  site to `only_me` with a required reason (a take-down: `restricted` in
  `access_decision`, audited `site_restricted`). The restriction is sticky:
  while it stands the owner or team cannot raise the level, request network
  access or add viewers (`409 site_restricted_by_admin` with the reason),
  nor rename the site or move it into a team. Only an admin
  lifts it (Lift, `unrestrict`), which restores the earlier level
  (`company` for a site that was on the network; `site_restriction_lifted`).
- **Status.** Built.
- **Routes.** `POST /api/sites/{sitename}/access`,
  `POST /api/collaboration/sites/{owner}/{sitename}/access`,
  `POST /api/admin/access-requests/{owner}/{sitename}/approve`,
  `POST /api/admin/access-requests/{owner}/{sitename}/decline`,
  `POST /api/admin/access-requests/{owner}/{sitename}/revoke` (decline and
  revoke take an optional `reason`),
  `POST /api/admin/sites/{owner}/{sitename}/restrict`,
  `POST /api/admin/sites/{owner}/{sitename}/unrestrict`.
- **MCP.** `set_site_access`; `get_site` and `list_sites` report
  `access_decision`.
- **Skill.** `references/collaboration.md` §6 (Who can open the site);
  `references/packaging-and-validation.md` (Who can open it);
  `simple-host-builder` §4.
- **Pages.** `/dashboard` access control per site, with the last admin
  decision; `/admin` "Access requests" (decline and revoke ask for a note)
  and Restrict / Lift on every site row.
- **Go.** `internal/handler/access.go`, `host_gate.go`
  (`requireHostSessionOrNetwork`, `serveSiteAPI`), `collaboration.go`
  (`network_request`); `internal/db/site_access.go`.
- **DB.** `sites.access`, `sites.network_requested_*` (0033);
  `network_access_approvals` (0038); `sites.public` (0006);
  `sites.access_decision*` (0047).
- **Config.** `NETWORK_ACCESS_APPROVALS` (1 or 2, default 1).

## 8. Named viewers (restricted sites)

- **What.** At level `specific` a site lists named viewers (people or teams);
  it is served on its own host like every site, and only
  listed viewers (and the owner or team) can open it. Viewers
  read, never write. Removing the last viewer keeps the level at `specific`,
  narrowing the site to its owner. A viewer may be named by company email
  (a plain ASCII address, letters, digits and `._%+-` before the `@`;
  refused outside `ALLOWED_EMAIL_DOMAINS` when set): the account whose
  IdP-verified address it is, or, if nobody has signed in with it yet, a pending viewer, listed with
  `pending: true` and the email as `username` ("hasn't signed in yet" on the
  dashboard), counted toward the 50, removed by that email, and converted at
  their first sign-in (section 1).
- **Status.** Built.
- **Routes.** `GET /api/collaboration/sites/{owner}/{sitename}/viewers`,
  `POST /api/collaboration/sites/{owner}/{sitename}/viewers`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}/viewers/{username}`,
  `GET /api/collaboration/sites/{owner}/{sitename}/viewer-candidates`.
  Host-gate: every path on the site's own host, including the nameless
  `/api/site/...` site API (or the fallback owner path, section 5). v1.2
  `<owner>--<site>.<base>` addresses redirect to the current address.
- **MCP.** `find_users`, `list_site_viewers`, `grant_site_viewer`,
  `revoke_site_viewer`.
- **Skill.** `references/collaboration.md` §7 (Named viewers).
- **Pages.** `/dashboard` viewers panel; "Shared with me" (section 5).
- **Go.** `internal/handler/viewers.go`, `host_gate.go`
  (`serveSiteHost`, `serveOwnerPath`, `serveLegacySiteHost`), `host.go`
  (`SplitSiteLabel`);
  `grant_emails.go` (email check); `internal/db/site_viewers.go`,
  `pending_grants.go`.
- **DB.** `site_viewers` (0024), `pending_site_viewers` (0045).
- **Config.** `MAX_SITE_VIEWERS`. See `docs/site-isolation.md`.

## 9. Teams

- **What.** A team is a namespace like a person, with one role: every member
  may do everything, including delete. Teams are named `team-<name>`:
  `POST /api/teams` and `create_team` accept `sales` or `team-sales`, both
  create `team-sales` (the response `name`), and `/api/teams/{team}/...`
  accepts either spelling. A person's handle never starts with `team-` (a
  derived `team-alpha` becomes `teamalpha`). Migration 0041 renamed older
  teams, refusing (naming them) any that would collide with an account's
  address or exceed 58 characters. Old team addresses redirect only while
  no account holds the old name: `sales.<base>/...` to `team-sales.<base>/...`
  (same path), `sales--<site>.<base>/` to the team site's current address.
  Created by any person (capped per
  person). When the last active member leaves, the team and all its sites are
  deleted — refused with `409 confirm_team_delete` and `site_count` until the
  call carries `?confirm_name=<team>`; delete works the same way. That 409
  offers the way to keep sites: move each first with `transfer_site` (to
  another team the person is in), then leave. A team whose
  members are all disabled is deleted by an admin, who can first move its
  sites to another team or a person (section 13). A team has no key and no
  sign-in. A member may be named by company email (a plain ASCII address;
  refused `invalid_email` outside `ALLOWED_EMAIL_DOMAINS` when set): the account carrying it, or a
  pending member (listed with `pending: true`, the email as `username`,
  counted toward the 50, removed by that email) who joins at their first
  sign-in (section 1).
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
- **Pages.** `/dashboard` "Teams": create a team, and per team its members
  (pending ones marked), add by username or email, remove, Leave (as the
  last active member, the server's `confirm_team_delete` is put to the person
  and the team name typed back), Delete (team name typed back); `/admin`
  orphan-team delete.
- **Go.** `internal/handler/team.go` (`teamName`), `admin.go`
  (`deleteOrphanTeam`), `auth.go` (handle prefix), `owner_index.go`,
  `host_gate.go` (`currentOwnerLabel`), `grant_emails.go`;
  `internal/db/teams.go` (`TeamPrefix`, `LegacyTeamName`), `pending_grants.go`.
- **DB.** `team_members`, `team_audit`, `users.kind` = `team` (0019); 0041
  renames teams to `team-<name>` (rows only, marked backward-compatible);
  `pending_team_members` (0045).
- **Config.** `MAX_TEAMS_PER_PERSON`, `MAX_TEAM_MEMBERS`.

## 10. Saved state and its history

- **What.** Each site has one JSON document its pages read and write through
  the site-facing API on the site's own host: plain (last write wins) or
  versioned (`state/versioned`, compare-and-set on a version). Anyone who may
  open the site may write it — deliberate: every viewer is a signed-in person.
  Every write keeps a history row; the last 20 per site are kept and any one
  can be restored by the owner or a team member.
- **Status.** Built.
- **Routes.** Host-gate: `GET|PUT /api/sites/{site}/state`,
  `GET|PUT /api/sites/{site}/state/versioned` on the site's own host (named
  only when it names that site) or on the fallback owner host, and the
  nameless `/api/site/state[...]` on every site host. Mux:
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
  per-site byte and count limits; asset bytes also count toward the owner's
  `QUOTA_MAX_BYTES` (413 `storage_quota`), and with `CLAMD_ADDR` set each
  upload is scanned before it is stored (422/503). Owners list and delete assets from the
  dashboard and from a chat app through base-host mirror routes.
- **Status.** Built.
- **Routes.** Host-gate, on the site's own host: `GET|POST /api/sites/{site}/assets`,
  `DELETE /api/sites/{site}/assets/{id}` (or nameless `/api/site/assets[...]`),
  serve at `GET /_assets/{id}[/{name}]`; on the fallback owner host the named
  routes and `GET /{site}/_assets/{id}[/{name}]`. Mux: `GET /api/collaboration/sites/{owner}/{sitename}/assets`,
  `DELETE /api/collaboration/sites/{owner}/{sitename}/assets/{id}`.
- **MCP.** `list_site_assets`, `delete_site_asset` (over the two mux routes;
  owner or team member; a publish key may list but not delete, on either
  host: a deleted file cannot be brought back, so deleting is `keyFull`,
  and the host gate's site API classifies its DELETE as
  `auth.SiteAPIDeletePattern`).
- **Skill.** `references/state-and-ai.md` (Uploaded assets);
  `simple-host-builder` §3.
- **Pages.** `/dashboard` assets panel.
- **Go.** `internal/handler/site_api.go`, `assets_admin.go`, `host_gate.go`,
  `upload_limits.go`; `internal/storage/assets.go`; `internal/db/assets.go`,
  `quota.go`; `internal/scan/clamd.go`.
- **DB.** `site_assets` (0026).
- **Config.** `ASSET_MAX_FILE_BYTES`, `ASSET_MAX_SITE_BYTES`,
  `ASSET_MAX_SITE_COUNT`, `QUOTA_MAX_BYTES`, `CLAMD_ADDR`, `CLAMD_TIMEOUT`.

## 12. Audit log, access log, export, retention

- **What.** `audit_events` records every mutation (most inside the mutation's
  transaction) and `access_denied`; `access_log` records visits through a
  batching best-effort writer. Both partitioned monthly. Every audit event
  is hash-chained in `audit_chain` by an AFTER INSERT trigger (tamper
  evidence; `access_log` is not chained), and `simple-host audit-verify`
  walks the chain, recomputing every hash in Go (never through the
  database's own function), and exits non-zero at the first break. Every
  event is also written to stdout, after its transaction commits, as one
  JSON line with `"type":"audit"` carrying its chain `seq` and `hash`, for a
  cluster log shipper to forward to a SIEM (which then anchors the chain
  outside the database; drops are counted in
  `simplehost_audit_stream_dropped_total`). `GET /api/audit` and
  `GET /api/access` are scoped to the caller's namespaces (admins see all,
  from a browser session only: an admin's API key or connected app gets the
  same own-namespace view as anyone else);
  access-log detail follows `ACCESS_LOG_VISIBILITY`. `/api/audit` filters by
  `owner`, `site` (under the owner; a site in Recently deleted still
  matches), `actor`, `action`, `from` and `to` (RFC 3339 or `YYYY-MM-DD`,
  inclusive) and pages by `cursor`, and each event carries `owner_name`,
  `site_name` and `actor_name`. An owner or team member is given
  `actor_name` for changes to the site: by themselves, a member of the
  owning team, or anyone who saved its data (`state_write`; the same person
  the dashboard shows as "written by" on each saved-data version). Someone
  who only opened the site, or was refused (`access_denied`), is never
  named, and for those rows `actor_id`, `key_id`, `ip` and `user_agent` are
  left out too; an admin always sees all of it. A non-admin's `actor` filter
  matches only themselves or a member of one of their teams (anyone else
  gives an empty page). `site_name` is the site's current name, given only while
  the site is still in the event's namespace (a site handed to a team is not
  named in its old owner's history). MCP `site_activity` reads one site's versions, its audit events
  (with `actor_name`) and its visit counts in one call; a part the
  credential cannot read (a publish key; `ACCESS_LOG_VISIBILITY=admin`)
  becomes a note, not a failure. `summary=counts` on `/api/access` asks
  for the counts shape under `owner` too (and, with `owner`, for an admin).
  The counts shape carries `top_pages` and `top_referrers`: people's page
  views (GET, 2xx/3xx, not bots, not assets) by path and by referring
  domain, at most 10 each. `access_log.referrer_domain` (0056) is the
  linking page's host only, never its path or query; '' for none or the
  same host; another page on this install is `<base>` or `*.<base>`, so a
  site's name never reaches another owner; only hostname characters
  `[a-z0-9.-]` are kept (punycode for international names, at most 253),
  an IP address or anything else is `(other)`, and `site_activity` tells
  the agent these are visitor-supplied data (`internal/handler/referrer.go`).
  Rows (admins; owners under `owner`) and exports carry it too. /admin's Activity card searches with those filters, loads more by
  cursor, and its export links carry them. Admins export either as
  CSV (formula-safe, with the three name columns last) or NDJSON; the export
  takes the same filters (`owner` and `site` for the access log). `simple-host prune` (a daily CronJob) drops
  partitions past retention (so a row lives its retention plus up to one
  month), trims the chain's rows for them, and keeps partitions twelve
  months ahead; rows that reached a default partition while it was not
  running are moved into their months' partitions, unchanged and still
  chained, instead of wedging every later run (0046).
- **Status.** Built.
- **Routes.** `GET /api/audit`, `GET /api/access`, `GET /api/admin/export`.
- **MCP.** `site_activity`.
- **Skill.** None.
- **Pages.** `/dashboard` and `/admin` activity/visitor panels (a site's
  Activity names who made each change).
- **Go.** `internal/audit/` (`stream.go`, `audit.go`, `db_recorder.go`, `access_writer.go`,
  `reader.go`, `prune.go`, `chain.go`, `canonical.go`, `commit.go`); `internal/handler/audit_access.go`,
  `audit_helpers.go`, `admin_export.go`; `internal/db/audit.go`, `audit_names.go`;
  `cmd/server/subcommands.go` (`prune`, `audit-verify`); `cmd/server/main.go`
  (the shared stdout JSON logger).
- **DB.** `audit_events`, `access_log` and their `_default` partitions (0027,
  0028, 0030); `audit_chain`, `audit_chain_head`, `audit_event_canonical()`,
  `audit_chain_append()` and trigger `audit_events_chain` (0036, owner-only);
  `audit_chain_entry()` (0040, the app role's read of one event's seq and
  hash for its SIEM line); `audit_ensure_partitions()` rewritten and
  `audit_ensure_month_partition()` (0046); `access_log.referrer_domain`
  (0056).
- **Config.** `AUDIT_RETENTION_DAYS`, `ACCESS_LOG_RETENTION_DAYS`,
  `ACCESS_LOG_VISIBILITY`. The stream and the chain have no settings.

## 13. Admin page

- **What.** Server-rendered `/admin` for admins: users (disable/enable —
  disabling revokes sessions, API keys and connected apps in the same
  transaction as its audit row; "Rename…" changes a person's address after
  a name change: the username, which is the label in every address, becomes
  the new name, their sites (same ids) answer under it, every old
  `<site>.<old>.<base>` address and the pre-v1.3 forms redirect through
  `site_redirects` (only for people who may open the site), the old owner
  page answers as a missing one (it never names the new name), the owner-hosts reconciler requests the
  new label's certificate (sites answer at `<new>.<base>/<site>/` until it
  is ready) and keeps the old one's for the redirects, search reindexes
  their sites, their Visitors history carries over (`/api/access` for the
  new name also counts what was recorded under the names they had), and the old label is held in `renamed_owner_labels` so no
  sign-in or rename takes it (the person may take it back); refused for a
  team, a name another person or team has, one held after a rename or an
  erasure, a team's pre-v1.3 address, `team-` names, dots, and anything
  that is not a valid label; audited `admin_rename_user` with `from`/`to`;
  erasure holds every old label too), orphan teams, a disabled person's or orphan
  team's sites ("Move to team…" moves them all to a team or person, all or
  none; "Delete sites" for a disabled person), a disabled person's data
  ("Export data": one zip of their account, teams, viewer grants, key,
  connected-app and session metadata, grants waiting for their email, their
  own visits, every site with its live files, saved data and history,
  versions and assets, and their audit events; "Delete person and all data":
  typed-username confirm, everything erased for good except audit rows,
  name held, old addresses of sites they handed on stop redirecting, and
  sign-in refused until an admin allows it again from the "Erased people"
  card), access requests, recently
  deleted sites (Restore, section 5), rankings of users
  and sites (views, storage from a cached bucket measurement, updated; each
  site links to its current address), new
  users, state-backend usage, visitors and activity, all sites (each with
  Restrict, or the restriction's reason and Lift: section 7). "Revoke a
  leaked key": paste any person's key (a password field; never echoed, logged
  or stored), that key alone is revoked and the answer names its owner, key
  name and last four; audited `admin_key_revoke` with the owner's name.
  "This instance" (top of the page): release, commit and schema; migrations
  waiting (`migrate.Pending`); this replica's latest bucket check and its
  time; owner certificates ready and waiting, each waiting owner with how
  long (`OWNER_CERTS=auto`; `manual` says the operator provides them); and
  the effective limits (quotas, versions kept, uploaded files, session and
  connected-app lifetimes, API key maximum, scanner, envelope, network
  approvals, audit/access-log/Recently-deleted retention).
- **Status.** Built.
- **Routes.** `GET /admin`, `POST /api/admin/users/{username}/disable`,
  `POST /api/admin/users/{username}/enable`,
  `POST /api/admin/users/{username}/rename` (form or JSON `name`; 409 on a
  collision, 400 for a name that cannot be an address),
  `POST /api/admin/users/disable` (offboarding by `{"email"}`: every person
  account with that address, idempotent, 404 when none; an admin's session
  or an admin's `offboard` key);
  `POST /api/admin/sites/{owner}/{sitename}/transfer`,
  `POST /api/admin/users/{username}/transfer-sites`,
  `POST /api/admin/users/{username}/delete-sites` (form or JSON `to`; only
  for a disabled person or a team with no active member, 409 otherwise;
  audited `site_transfer` / `site_delete` with `by_admin`);
  `GET /api/admin/users/{username}/export` (zip, streamed; audited
  `admin_user_export`), `POST /api/admin/users/{username}/erase` (form or
  JSON `confirm` = the username; only for a disabled person, 409 otherwise
  or while they are a team's last member; audited `user_erased` by id only,
  plus `site_delete` with `erasure` per site);
  `GET /api/admin/erased-identities`,
  `POST /api/admin/erased-identities/{id}/allow` (audited
  `erased_identity_allowed`); `POST /api/admin/keys/revoke` (JSON `key`;
  200 `revoked` or `already revoked` with owner, name, label; 404 when no
  key matches); plus the admin
  routes in sections 7, 9, 12 (section 7 has restrict and unrestrict).
- **MCP.** None.
- **Pages.** `/admin`.
- **Go.** `internal/handler/admin.go` (`leaverSiteActions`,
  `personDataActions`, `renameUserAction`), `admin_move.go`, `admin_erase.go`,
  `admin_rename.go`, `internal/db/erase.go`, `internal/db/rename.go`,
  `admin_rankings.go`, `admin_disk_usage.go`, `admin_keys.go`,
  `admin_status.go` (filled by `cmd/server/status.go`), `access.go`
  (`renderAccessRequests`), `site_restore.go` (`renderDeletedSites`).
- **DB.** `users.disabled_at` (0023), `site_daily_analytics` (0003, 0013),
  `erased_owner_labels`, `erased_identities`, the
  `users_refuse_erased_label` trigger and `access_log_erase_visitor()`
  (0048); definer functions' search path pinned (0049);
  `renamed_owner_labels` and the `users_refuse_renamed_label` trigger, and
  the `owner_label_lock` advisory lock both label triggers, renames and
  erasures take (0057).
- **Config.** `ADMIN_EMAILS`, `OIDC_ADMIN_CLAIM`, `OIDC_ADMIN_VALUE`. See
  INSTALL.md "Sessions and leavers" and docs/configuration.md "Data subject
  requests".

## 14. Dashboard

- **What.** `/dashboard` on the base host: sign-in prompt when signed out;
  when signed in, API keys (mint/list/revoke; a new key stays on screen
  until dismissed), "Your sites" across the
  person's and their teams' namespaces, each name linking to the site's
  address with its live version and last update, with access level (and the last
  admin decision: declined, revoked or restricted, with the note), viewers, assets,
  versions with Make live (rollback with the listed ETag; a 412 says to
  reload), saved-data history with Restore (the newest is marked current),
  Download site (section 5), rename and hand over (section 5), Delete (the
  site name typed back; it goes to Recently deleted), Visitors (counts per
  day, the top pages and where visitors came from by domain, over 30 days;
  rows too where `ACCESS_LOG_VISIBILITY` shows them), each namespace's usage against its quota (sites, stored
  bytes; the same numbers `GET /api/me` returns as `usage`), "Teams"
  (section 9), "Shared with
  me" (section 5; hidden when empty), "Recently
  deleted" (the person's and their teams' sites deleted within `DELETED_RETENTION_DAYS`,
  each with Restore; hidden when empty), "Not used lately" (sites the idle
  cleanup marked, with the date each moves, Keep and Download; section 5;
  hidden when none), and a link to sessions. Key rows show the key's last
  four ("earlier key" before 0050), last used, and "expires soon"; a
  site's Activity names who made each change (section 12). With no
  sites, the list shows the connect step instead: the `/mcp` address,
  `plugin.zip` and the install page. Calls the JSON routes of sections
  2, 5, 7, 8, 9, 10, 11 and 12 with the session cookie.
- **Status.** Built.
- **Routes.** `GET /dashboard`.
- **MCP.** None.
- **Skill.** `references/account-recovery.md` (Get a key).
- **Go.** `internal/handler/dashboard.go`, `upload_limits.go`.
- **DB.** Reads only. **Config.** `SESSION_IDLE`, `QUOTA_MAX_SITES`,
  `QUOTA_MAX_BYTES`.

## 15. Search, showcase, owner index

- **What.** Search indexes the text of `listed`/`network` sites (a background
  worker re-extracts on every deploy, rollback and delete; PostgreSQL full
  text) and answers signed-in callers, linking each result to the site's
  address at index time (documents indexed before v1.3, or before the owner
  was ready, carry owner-host URLs, which redirect; each site's next deploy
  reindexes it); clicks and impressions are recorded
  pseudonymously and pruned by a background loop. `/showcase` is the gallery
  of listed sites; its search box filters by owner and site name as you type
  and adds the sites `GET /api/search` finds by the words on their pages
  (after a pause in typing, or on submit; the name filter alone when search
  fails). MCP `search_sites` is the same search for agents. The root of an owner host (`GET /` on `<owner>.<base>`,
  sign-in required) is the owner's index page, linking to each site's
  current address: everything to the owner (for a team, to each of its
  members), only listed sites to others, never a
  `specific` one. A pre-v1.3 team address redirects to `team-<name>`.
- **Status.** Built.
- **Routes.** `GET /api/search`, `POST /api/search/click`, `GET /showcase`.
  Host-gate: `GET /` on an owner host.
- **MCP.** `search_sites`.
- **Skill.** `references/state-and-ai.md` (Public search).
- **Pages.** `/showcase`; owner-host index.
- **Go.** `internal/handler/search.go`, `showcase.go`, `owner_index.go`;
  `internal/search/` (`worker.go`, `extract.go`, `public.go`,
  `telemetry_pruner.go`); `internal/db/search.go`, `search_query.go`.
- **DB.** `site_search_documents`, `site_search_queue`,
  `site_search_index_status`, `site_search_queries`,
  `site_search_impressions`, `site_search_clicks` (0011).
- **Config.** `SEARCH_TELEMETRY_RETENTION_DAYS`, `SEARCH_SESSION_MAX_AGE`, `RATE_LIMIT_SEARCH_QUERY_PEER`, `RATE_LIMIT_SEARCH_QUERY_SESSION`.

## 16. Metrics, health, request log, rate limits

- **What.** `/healthz` (liveness) and `/readyz` (database and schema; bucket is
  reported, not gating) on every host. `/metrics` on its own port, never on
  the Service or Ingress: request counts and latency, `simplehost_bucket_ok`,
  `simplehost_config_warning` (startup checks, section 18),
  `simplehost_owner_hosts_not_ready` (section 19), DB pool, build info. Structured request log. Rate limits and concurrency
  slots: sign-in, session hand-off, API key mint and the connector's token
  and registration limits are counted in Postgres and shared by every
  replica; every other limit is per pod in memory (`docs/install.md`,
  "Rate limits and replicas"). Per-site daily views (bot traffic split out; deploy-check
  traffic via `CheckHeader` not counted as human) and file downloads.
- **Status.** Built.
- **Routes.** `GET /healthz`, `GET /readyz`, `GET /metrics` (metrics port).
- **MCP.** None.
- **Go.** `internal/handler/health.go`, `abuse_limits.go`, `client_kind.go`,
  `download_recorder.go`; `internal/metrics/`; `internal/reqlog/`;
  `internal/ratelimit/` (`limiter.go` per pod, `shared.go` shared).
- **DB.** `site_daily_analytics` (0003, 0013), `site_file_downloads` (0009),
  `rate_limit_counters` (0039).
- **Config.** `METRICS_PORT`, `PORT`, `HTTPS_REDIRECT_PORT`, `SECURE_MODE`,
  `TRUSTED_PROXY_CIDRS`, `RATE_LIMIT_<NAME>` (`<burst>/<interval>`, one per
  limiter), `UPLOAD_CONCURRENCY`.

## 17. Landing and static pages

- **What.** The base host serves embedded static pages and the API spec.
- **Routes.** `GET /` (file server: `docs.html`,
  `capabilities.html`, `install.html`, `changelog.html`, fonts, CSS);
  `GET /{$}` (`index.html`) and `GET /openapi.yaml`, served with the
  installation's operational values filled in (section 18).
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
  `OAUTH_REFRESH_TTL`, `OAUTH_REFRESH_TTL` over 90 days; an operational
  time or limit (`DELETED_RETENTION_DAYS`, `PREVIEW_LINK_TTL`,
  `MAX_ARCHIVE_BYTES`, the rest in `docs/configuration.md`, "Operational
  times and limits") outside its range; a `RATE_LIMIT_*` of the wrong shape,
  a security-sensitive limit more than 4 times looser than its default
  (`handler.SensitiveRateLimit`), or a shared window over 30 minutes (an
  unknown `RATE_LIMIT_*` name, or another limit over 10 times looser, only
  warns); clashing ports; `DB_APP_PASSWORD` equal to
  `DB_PASSWORD`; `DB_APP_USER` combined with `DB_DSN`; a schema newer than the
  binary unless every newer migration is marked backward-compatible.
  Startup also warns (log line and `simplehost_config_warning{check}`),
  without refusing, when no admin is configured (neither `ADMIN_EMAILS` nor
  `OIDC_ADMIN_CLAIM`) and when the bucket reports versioning not enabled; a
  provider that cannot report versioning is logged as unknown, not flagged.
  `simple-host migrate` applies the schema and sets the least-privilege
  `simplehost_app` role's password; the server connects as that role.
- **Subcommands.** No argument runs the server. Others: `migrate`, `restore`, `migrate-storage`,
  `reencrypt` (section 6), `prune`, `audit-verify` (section 12), `owner-hosts`
  (section 19), `settings --json` (below), `version`.
- **Settings registry and advanced docs.** `internal/config/settings.go` lists
  every variable `internal/config` reads with its area, a plain description,
  type, default, range, whether it is security-sensitive, required, or asked
  in basic setup (a test fails when a read variable is missing);
  `simple-host settings --json` prints it with the rate-limit defaults from
  `internal/handler`, and `docs/advanced/settings.json` is that output (a
  `cmd/server` test fails on drift). `docs/advanced/` explains each area with
  recipes; its tables are filled from that file and `docs/configuration.md` is
  checked against it (`scripts/settings_docs.py --check`, in `make test` and
  CI). The hosted repo's setup helper (simple-host.app/setup, Enterprise)
  reads a copy of the file to write `config.env`, a `secrets.env` template and
  the apply commands.
  The operational values live in `internal/oplimits`, set once at startup
  (`cmd/server` `loadConfig`); every surface that states one (dashboard,
  `/admin`, idle email, refusals, MCP descriptions, served skills,
  `/openapi.yaml`, the home page) reads it there, the static documents
  through `{{NAME}}` placeholders.
- **Go.** `internal/config/config.go`, `oplimits.go`, `settings.go`; `internal/oplimits/`; `internal/migrate/`;
  `cmd/server/main.go`, `subcommands.go`, `settings.go`.
- **DB.** `simplehost_app` grants (0020); every migration.
- **Config.** `DB_DSN` or `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`,
  `DB_PASSWORD`, `DB_SSLMODE`, `DB_SSL_ROOT_CERT`, `DB_APP_USER`,
  `DB_APP_PASSWORD`, `DB_INSECURE_ALLOWED`, `DB_INCLUSTER_EVALUATION`,
  `OWNER_CERTS` (`auto` or `manual`, refused otherwise); all of
  the above. Full list: `docs/configuration.md`.

## 19. Owner hosts: per-owner certificates

- **What.** A TLS wildcard covers one label, so `<site>.<owner>.<base>` needs
  a `*.<owner>.<base>` certificate per owner (the existing `*.<base>` DNS
  record already resolves every depth; no per-owner DNS). With
  `OWNER_CERTS=auto` (default) a separate Deployment runs
  `simple-host owner-hosts` every `OWNER_HOSTS_INTERVAL` (15 s): for each
  owner with at least one site, and each owner label a moved or renamed site
  still redirects from (`site_redirects`), it server-side-applies one Ingress
  `sh-owner-<owner>` (host `*.<owner>.<base>`, TLS secret
  `sh-owner-<owner>-tls`, annotation `cert-manager.io/cluster-issuer:
  <OWNER_CERT_ISSUER>`), copying ingress class, controller annotations and
  backend from the install's own Ingress (`OWNER_INGRESS_TEMPLATE`);
  cert-manager's ingress-shim issues the certificate. The reconciler reads
  the Certificate's Ready condition (never the Secret) and records it in
  `owner_hosts`. An owner with no sites left loses its row first, then its
  Ingress (the Certificate goes with it; the TLS Secret is left behind).
  The server reads `owner_hosts` through a per-replica cache refreshed every
  15 s; until an owner is ready its sites are served at the fallback
  `<owner>.<base>/<site>/` (section 5), so nobody is sent to a host without a
  certificate. Without the reconciler deployed, everything keeps serving at
  the fallback. With `OWNER_CERTS=manual` the component is left out, every
  owner is treated as ready, and the operator provides each owner's
  certificate and Ingress host rule. The server's pods keep no Kubernetes
  credential; the reconciler's ServiceAccount is the one token in the
  install (namespaced Role: ingresses get/list/create/patch/delete,
  `certificates.cert-manager.io` get; no Secrets). It connects to Postgres
  as the application role. Readiness is visible without kubectl: /admin's
  "This instance" card counts ready and waiting owners and lists each
  waiting one with how long (`db.OwnerHostReadiness`), and
  `simplehost_owner_hosts_not_ready` counts them.
- **Status.** Built.
- **Routes.** None. **MCP.** None. **Skill.** None.
- **Go.** `internal/ownerhosts/ownerhosts.go`; `internal/handler/owner_hosts.go`
  (`OwnerHostReadiness`), `host.go` (`WithOwnerReadiness`, `OwnerReady`,
  `SiteURL`); `cmd/server/subcommands.go` (`runOwnerHosts`),
  `cmd/server/main.go`; `internal/config/config.go` (`LoadOwnerHosts`).
- **Deploy.** `deploy/components/owner-hosts/` (`deployment.yaml`,
  `rbac.yaml`, `serviceaccount.yaml`), included by the byo, production,
  staging and local overlays.
- **DB.** `owner_hosts` (0042, backward-compatible); reads `site_redirects`
  (0043).
- **Config.** `OWNER_CERTS` (server), `OWNER_CERT_ISSUER` (required by the
  reconciler), `OWNER_INGRESS_TEMPLATE` (default `simple-host`),
  `OWNER_HOSTS_INTERVAL` (default 15s). See `docs/site-isolation.md`.

---

## Orphans and dormant surface

- Columns with no reader or writer left in the code: `sites.site_type`,
  `sites.site_type_version` and their index (0014). Kept one release after
  the classifier's removal so a rollback still finds them; drop them next.
- `reset_requests`, `key_reissues` and `ai_usage` are dropped by 0034.
- No MCP tool is orphaned: every tool resolves to a route above.
