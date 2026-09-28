# Changelog

Releases are published as `ghcr.io/vineetu/simple-host-enterprise:<version>`;
pin the digest, not the tag. `simple-host version` prints the running
release, commit and schema.

## v1.9.1 — 2026-09-28

### Changed
- AWS quick setup: every question on the setup page can be left empty.
  `apply.sh` asks for what the line does not have (address, admin emails,
  issuer, client ID, company email domains with Google, an existing
  cluster's name) in the shell, in order, and adds the answers to
  `terraform.tfvars` and its saved copy; with no terminal or with `--yes` it
  stops and lists them. `admin_emails` accepts an internal domain without a
  dot (`platform@corp`). `scripts/test-ask.sh` runs the questions at a
  pseudo-terminal. Answers are remembered in
  `~/simple-host/last-answers.tfvars` (and the saved copy for the address),
  so pasting the line again after CloudShell closed needs only Enter: each
  earlier answer is the question's default. Answers are split without
  globbing (`*@example.com` is never matched against files), and an answer
  with an invisible character (zero-width, direction marks, byte order
  mark, any Unicode format character) or a control character is refused.

### Fixed
- A blank `ADMIN_EMAILS` (with no `OIDC_ADMIN_CLAIM`) no longer demotes every
  admin of an install that has them: the startup sync and the sign-in refresh
  leave admin status unchanged and the server logs a warning. Before, an
  empty list set `is_admin = false` for everyone at the next start.

## v1.9.0 — 2026-09-28

### Added
- Terraform quick setup for AWS: `deploy/terraform/aws` sets up a new EKS
  cluster (or installs into yours), RDS for PostgreSQL, the S3 bucket,
  generated secrets in AWS Secrets Manager synced by External Secrets, IAM
  roles for service accounts, cert-manager and Traefik behind an NLB, a
  Route 53 zone with Let's Encrypt certificates for the address and every
  owner, and Simple Host at the pinned release. `apply.sh` runs it from AWS
  CloudShell with remote state in the account; https://simple-host.app/setup
  gives the one line. Running the line again picks up where a stopped run
  left off (CloudShell closes after about 20 minutes without a key press):
  it stops Terraform cleanly on a hang-up, clears a stale lock, and keeps or
  adopts what a cut-off run made. A new image digest or setting rolls the
  pods without touching the namespace or certificates. On an existing
  cluster it uses a cert-manager or External Secrets already there (and
  stops with the fix when only their CRDs are left or they are too old;
  `scripts/test-detect.sh` checks this on a kind cluster), and never removes
  the cluster's IAM OIDC provider. `--destroy` removes everything it made,
  its state bucket included. Docs: `docs/cloud/aws.md`, the module's
  README.

### Fixed
- Dashboard: the MCP address box (and the new-key box) had light text on a
  light background; the text is dark now.

## v1.8.3 — 2026-09-28

### Fixed
- Connecting an AI app (ChatGPT, Claude, Cursor and others) through the
  browser works again. Clicking **Allow** on the consent screen returned
  `{"error":"forbidden"}`: the page's `Referrer-Policy: no-referrer` made
  the browser post the form with `Origin: null`, which the same-origin
  check refuses. The consent screen now sends `same-origin`, which still
  sends nothing to other sites.

## v1.8.2 — 2026-09-28

### Fixed
- A person whose address is a reserved name, most often an admin at
  `admin@acme.com`, signs in as `admin-acme` (the name plus the first part
  of the email's domain) instead of `admin-2`, which read as if an
  `admin-1` existed. A name another account holds or held still gets a
  number. The dashboard notice now says the usual name is reserved or
  taken.
- The owner-hosts reconciler no longer logs
  `list owners: pq: password authentication failed for user "simplehost_app"`
  while it starts alongside the migration that sets that role's password.
  For its first 3 minutes a database error logs one "waiting for the
  database schema and application role" line; only an error that outlasts
  that, or comes after the database has answered once, is logged as such.
- `make smoke` against an install whose owner certificates come from an
  internal CA the running machine does not trust stops at once with "the
  certificate for `<site host>` is not trusted by this machine" and how to
  fix it, instead of waiting 5 minutes and reporting the host "not ready".
  A timeout now names curl's last error. INSTALL.md section 9 shows running
  it with `CURL_CA_BUNDLE` (the system bundle plus the CA).
- INSTALL.md section 4 says which `config.env` keys come from the example
  file and which the setup helper leaves to their defaults (do not copy the
  rest of the example into a helper's file), and which `secrets.env` keys the
  generator fills. "Done" and the HUMAN STEPS point to HUMAN STEP D and
  `make smoke` in sections 8 and 9.

## v1.8.1 — 2026-09-27

### Fixed
- A signed-out colleague who opens a shared link to a site that needs
  sign-in is sent to company sign-in and back to the site, instead of a raw
  `{"error":"unauthorized"}` page. Scripts, agents and API-key calls still
  get the 401 JSON. The hand-off's nonce cookie now lasts 10 minutes, long
  enough for a sign-in with a second factor.
- An API key calling an admin route is told that admin routes need a
  browser sign-in, instead of being told to mint a key with another scope
  (no key can call them). An offboard key is told the one route it calls.
- `simple-host migrate -status` works through `kubectl exec` in the
  server's container (it reads the schema as the application role).
  `audit-verify`, `prune` and applying migrations need the owning role,
  which that container does not hold: run through `kubectl exec` they now
  say so and name `make job ARGS="..."`, a new target
  (`scripts/run-job.sh`) that runs any subcommand as a one-off Job cloned
  from the prune CronJob, follows its output and deletes it. The restore
  drill, INSTALL.md and `docs/security-review.md` use it.
- `docs/uninstall.md`: the load balancer belongs to the ingress
  controller's Service and keeps billing after the `simple-host` namespace
  is gone; the page now says so and gives the commands to remove
  ingress-nginx and cert-manager (or just the Service). A new
  `scripts/bucket-delete-versions.sh` deletes every version and delete
  marker under a prefix on any S3-compatible store (the secret read from a
  0600 curl config file). Every `kubectl` line names its context, and the
  DNS check allows for a parent zone's own wildcard.
- `docs/cloud/upcloud.md`: use the database's `public-` hostname (the
  plain one resolves to a private address from outside UpCloud); API
  examples to create the database and read its host and admin password into
  variables, with a warning that `upctl database show` prints that password;
  reading the project CA from the server's TLS chain with a fingerprint
  check; setting the owning role's password with no terminal through a new
  `scripts/db-role-password.py` (sends only a SCRAM verifier); a
  bucket-only IAM policy through the API instead of `ECSS3FullAccess`; the
  bucket's region; annotating the load balancer straight after
  `make prereqs`; and why a node-IP database filter breaks when nodes
  change. `docs/storage.md`'s bucket `curl` commands read the key pair from
  a 0600 file (`-K`), not the command line.
- Calls with an API key must name their client: an agent's skill sends
  `X-Skill-Version`, and a script or pipeline sends
  `X-Simple-Host-Client: api`. `docs/ci.md`'s publish commands now send
  it (without it they got `400 skill_version_required`), INSTALL.md and
  `docs/install.md` say so, and the 400's message names both headers.
  `docs/ci.md` also says a publish key cannot delete uploaded files or
  download a whole site.
- The settings registry marks `DB_INCLUSTER_EVALUATION` as security and
  data-safety sensitive (it marks the database as throwaway), so
  `simple-host settings --json` and the setup check flag it.
- INSTALL.md says an admin's username may carry a suffix (`admin-2`) when
  the name is reserved or held, and names both image-digest placeholders;
  `docs/cloud/upcloud.md` says an out-of-band base certificate is the
  operator's to renew every 90 days, with the Secret update command.
- `make smoke` against a real install checks the key's scope first and
  stops before publishing when it is not Full, instead of failing four
  checks and leaving its throwaway site behind; if the site cannot be
  deleted on exit it says so. INSTALL.md HUMAN STEP D now says to mint the
  key with scope Full.
- The dashboard's key help says what a publish key cannot do: delete
  uploaded files or download a whole site (a full key can).

## v1.8.0 — 2026-09-27

Schema 0058 (backward-compatible); run `simple-host migrate` before
deploying. Skills are at 0.15.5 (0.11.0 still works).

### Security
- Deleting an uploaded file from the site's own address
  (`DELETE /api/sites/{site}/assets/{id}`, `/api/site/assets/{id}`) now needs
  the person who uploaded it, the site's owner or a team member (403
  otherwise). Anyone who can open the site still uploads and lists; a deleted
  file cannot be brought back, so opening the site is no longer enough.
- `GET /auth/sessions` (your browsers and connected apps) now refuses an
  API key like the other session-only routes.
- A whole-site download link stops working when the session, API key or
  connected app that asked for it is revoked, signed out everywhere or
  disconnected. Asking for a link is audited (`site_export_link`), and the
  download is recorded as the key when a key asked for it. Links asked for
  before this release stop working (they last 10 minutes by default).
- Idle cleanup never marks or deletes a site an admin has restricted (a
  takedown often held as evidence); a mark it had is cleared.
- Erased people's sign-in identities and emails, and the owner identity in
  each site's bucket manifest, are now kept as HMACs under a key derived
  from `SESSION_SIGNING_KEY`, not plain SHA-256 values anyone with the
  database or a backup could match against the company directory. Rows and
  manifests written before still match. Keep retired signing keys in your
  secret store: an erased person recorded under a key you remove is no
  longer recognised at sign-in. Rolling back to v1.7 stays possible, but
  v1.7 does not recognise people erased after the upgrade (under it they
  could sign in again) and its `rebuild-index` refuses manifests written
  after it.
- Site manifests are signed. `simple-host rebuild-index` refuses one that
  was changed in the bucket, one signed under a key no longer configured
  (add it back as the second `SESSION_SIGNING_KEY` entry for the rebuild),
  and one written before this release unless you pass `-accept-unsigned`
  after reading the list. A manifest now records when a deleted site is
  purged, and a rebuild keeps that date instead of recomputing it from
  today's retention.
- An owner's activity names whoever saved their site's data, but no longer
  shows that person's IP address or browser unless they are the owner or a
  teammate.
- A refused preview no longer writes its link's token into the audit log.
- An admin's rename of a person and an admin's revoke of a leaked key now
  appear in that person's own activity (`/api/audit`).

### Docs
- `/openapi.yaml` is versioned with the release (1.8.0; it said 2.5.0) and
  now lists every route a key can call: creating and deleting a site by
  owner (`POST`/`DELETE /api/collaboration/sites/{owner}/{sitename}`),
  `POST /api/sites/{sitename}/keep`, a site's named viewers and viewer
  search, team member search, and offboarding by email
  (`POST /api/admin/users/disable`).
- `deploy/overlays/byo/config.env.example` and `secrets.env.example` list
  every setting, the optional ones commented out at their defaults.
- The Capabilities page mentions preview, Recently deleted (with this
  install's window), rename and move, and whole-site download.
- README, INTENT (decisions of v1.4 to v1.7 recorded), `docs/ci.md` (what a
  publish key can do), `docs/configuration.md` and FEATURES corrected.

### Configuration
- Safer lower bounds, refused at startup with a message naming the range:
  `IDLE_CLEANUP_DAYS` is 0 (off) or 30 to 3650, `IDLE_CLEANUP_GRACE_DAYS`
  and `DELETED_RETENTION_DAYS` are 7 to 365 (they were 1). An install that
  set a lower value must raise it before upgrading.

### Fixes
- Two download links asked for within the same second were the same link,
  so using one used up the other; each link is now distinct.
- A download link validated just before it expired could be used a second
  time once its first use had been swept; an expired link is now refused
  when it is used.
- Idle cleanup marks and unmarks one site at a time under the site's lock,
  so it no longer deadlocks with a bulk move or delete.
- A sign-in converting grants made to its email could deadlock with an
  erasure or offboarding of the same person, or with the purge of a
  deleted site holding such a grant. Conversion now locks the person (and
  whoever made the grants) before it takes any grant, and leaves grants on
  a site in Recently deleted pending.
- An erasure that found a team membership added a moment earlier could
  lock rows out of order and deadlock with a site move; it now starts its
  locks over in order.
- Renaming a person is refused (400) when one of their sites, Recently
  deleted included, would have an address longer than a hostname may be;
  it used to succeed and leave the site with no address.
- A site rename or hand-over that waited on a person being renamed used
  the name from before: the redirect was recorded for an address the site
  no longer had and the answer named the old owner. Names are now read
  under the move's locks.
- A deploy, rollback, saved-data write or upload by a person being renamed
  or erased at that moment could deadlock with the rename or erasure; each
  now takes the writer's row before the site's.
- Migrations 0047 (admin restriction) and 0048 (erasure) are now recorded
  as not backward-compatible (0058 corrects databases already migrated), so
  an image built before them is refused at startup instead of letting an
  owner reopen a restricted site or an erased person register again.
  Released images before v1.4.0 were already refused.
- Two audit partition upkeep runs at once (a prune overlapping another
  prune or a migrate) could deadlock; they now take turns (0058).
- `owner_label_lock` is no longer executable by every database role, only
  the application role (0058).
- A manifest write that started before a site was purged or erased could
  finish after the manifest was deleted and put it back; deletion now
  waits for it.

## v1.7.0 — 2026-09-27

Schema 0055–0057 (all backward-compatible); run `simple-host migrate`
before deploying. Skills are at 0.15.4 (0.11.0 still works).

### Renaming a person's address
- After a name change an admin presses **Rename…** on the person's row in
  `/admin` (`POST /api/admin/users/{username}/rename`). The new name becomes
  the label in all their addresses; their sites answer under it. Every old
  site address and its older forms redirect to the new one only for people
  who may open the site; the old owner page answers as a missing one and
  never names the new name (a name change is often sensitive); and the owner-hosts reconciler requests the new name's certificate
  (until it is ready the sites answer at `<new>.<base>/<site>/`). Their
  Visitors history carries over. The old
  name is held so no sign-in or rename takes it; erasing the person keeps
  every name they had held. Collisions (another person or team, a held
  name, a team's old address) are refused. Audited as `admin_rename_user`.
  A sign-in, rename or erasure touching a name takes a per-name lock, so a
  sign-in racing a rename waits and is refused. Schema 0057
  (backward-compatible: a binary before it keeps the names held, even
  through an erasure, but its pre-v1.3 team mapping can answer a renamed
  person's old owner page for a team created later with that name).

### Docs
- `docs/uninstall.md`: removing a real install, in order — what to export
  first (sites, the audit and access logs, a full database and bucket copy
  with the envelope key), the DNS records before the load balancer is
  released (a dangling `*.<base>` record lets someone else serve under the
  company's domain), the namespace and reconciler, a ClusterIssuer made
  only for this, the OIDC application, the bucket with its noncurrent
  versions, the database with its backups, and the secrets kept elsewhere.

### Rebuilding from the bucket
- Each site now keeps `sites/<id>/manifest.json` in the bucket: its owner
  (name, and a hash of a person's sign-in identity or a team's id; never an
  email), name, live version, whether it is deleted or restricted, and
  uploaded files' names and types, rewritten after every deploy, rollback,
  rename, hand-over, delete, restore, restriction and file change. If the
  database is lost together with its point-in-time recovery,
  `simple-host rebuild-index` lists every site the bucket can bring back,
  and `-apply` recreates each whose owner is found: same site id, every
  kept version, the live one live, uploaded files, deleted and restricted
  sites as they were. A person's sites go only to the account with the
  same sign-in identity, never to whoever has the username now; a team's
  need `-map <old>=<team>` once it is created again. Every field is checked
  as a create would check it, a site whose live version's archive is
  missing is refused rather than given a version nobody made live, and
  `-apply` refuses a database that already has sites unless
  `-force-live-db`. Saved data,
  access levels, viewers and team members were only in the database and
  are not recovered. A site gets its manifest at its next deploy (or
  rollback, rename or file change) after this release.

### Visitors: top pages and where they came from
- A site's Visitors view on the dashboard lists its most opened pages and
  the domains that linked to it over the last 30 days: counts only, by
  people (not bots or asset requests), following `ACCESS_LOG_VISIBILITY`
  like every other count. Only the referring domain is kept
  (`access_log.referrer_domain`, schema 0056, backward-compatible), never
  its path or query, and only as hostname characters (punycode for an
  international name); an IP address or anything else shows as `(other)`; another site on the same install shows as
  `*.<base>`. `GET /api/access` counts gain `top_pages` and
  `top_referrers`; `site_activity` returns them; rows and the admin export
  gain `referrer_domain`.

### Deploying from CI
- One call deploys from a pipeline: `PUT /api/sites/{sitename}?create=true`
  (or the team route `PUT /api/collaboration/sites/{owner}/{sitename}?create=true`)
  creates the site when it is missing (`201`) and updates it otherwise
  (`200`). Without the flag a missing site is still `404`. A create of a
  name the namespace already has now answers `409` with code `site_exists`
  and says which call would have worked. `docs/ci.md` has the recipe with a
  publish-scoped key; INTENT records CI deploys as supported (2026-09-27).

### Connected apps
- Two connections of the same app can be told apart: `/auth/sessions`
  shows, next to when each was connected and last used, the browser that
  allowed it ("from Chrome on macOS"), and `GET /api/me/connections`
  returns it as `device`. Only that summary is kept, worked out when Allow
  is pressed; never the raw browser header or an IP address. Connections
  made before this show no hint. Schema 0055 (backward-compatible).

### Configuration
- `simple-host settings --json` prints every setting with its area, a plain
  description, type, default, range, and whether it is security-sensitive
  or required; `docs/advanced/settings.json` is that output, checked in CI.
- `docs/advanced/`: every setting by area (server and addresses, accounts
  and sign-in, access and teams, sites, saved data, cleanup and retention,
  email, storage and backups, observability, rate limits), explained in
  product terms with recipes. The tables are generated, so they cannot drift.
- The setup helper at https://simple-host.app/setup (choose Enterprise)
  writes `config.env`, a `secrets.env` template naming every secret and the
  apply commands, in the browser; an optional check of your choices sends
  only the names and values of the numbers, durations, switches and rates
  you changed.

## v1.6.0 — 2026-09-27

Schema 0054 (backward-compatible: nullable `sites.purge_at` and
`sites.idle_delete_at`); run `simple-host migrate` before deploying. Skills
are at 0.15.2 (0.11.0 still works).

### Configuration
- Every operational time and limit an operator may reasonably change is now
  an environment setting, defaulting to the value it had before, and
  refused at startup when out of range (`docs/configuration.md`,
  "Operational times and limits"): `DELETED_RETENTION_DAYS` (30),
  `IDLE_CLEANUP_GRACE_DAYS` (30), `IDLE_CLEANUP_MAX_EMAILS` (0, no limit:
  with it set, one run marks and emails at most that many sites, the rest
  on later runs), `PREVIEW_LINK_TTL` (1h), `EXPORT_LINK_TTL` (10m),
  `API_KEY_DEFAULT_DAYS` (90), `API_KEY_EXPIRY_WARNING_DAYS` (14),
  `MAX_TEAMS_PER_PERSON` (10), `MAX_TEAM_MEMBERS` (50), `MAX_SITE_VIEWERS`
  (50), `MAX_ARCHIVE_BYTES` (100 MiB; raise the ingress body limit with it),
  `MAX_FILES_PER_SITE` (50,000), `UPLOAD_CONCURRENCY` (2),
  `SEARCH_TELEMETRY_RETENTION_DAYS` (180) and `SEARCH_SESSION_MAX_AGE`
  (4320h).
- Each rate limiter is set with `RATE_LIMIT_<NAME>=<burst>/<interval>`, for
  example `RATE_LIMIT_AUTH_EMAIL=5/50s`; another shape, or a limit counted
  across replicas whose window would exceed 30 minutes, refuses startup. An
  unknown `RATE_LIMIT_*` name (a typo, or another program's variable) is
  ignored with a startup warning. The security-sensitive limits (sign-in,
  the email code, API key mint, admin, the connector's registration and
  token endpoints) may be made stricter freely but at most 4 times looser
  than the default (startup names the ceiling); any other limit set more
  than 10 times looser logs a warning.
- A deleted site keeps the purge date it was given, and a site marked idle
  the delete date it was given (stored in the new columns), so changing
  `DELETED_RETENTION_DAYS` or `IDLE_CLEANUP_GRACE_DAYS` applies to new
  deletions and marks only. `docs/configuration.md` also gives the memory an
  upload needs (`UPLOAD_CONCURRENCY` × (`MAX_ARCHIVE_BYTES` + 500 MiB)) and
  notes that installed skills keep old numbers until reinstalled.
- What people and agents are told follows the setting: the dashboard,
  `/admin` (the "This instance" card lists the new values), the idle-cleanup
  email, refusal messages (team, member and viewer limits now name the
  number; a too-large upload names the limit), MCP tool descriptions and
  hints, the served skills and plugin, `/openapi.yaml` and the home page.

## v1.5.0 — 2026-09-27

Schema 0053 (from 0049), all four backward-compatible: nullable
`api_keys.last4` (0050), `sites.idle_since` and `sites.idle_keep` (0051), an
index on `site_viewers (principal_id)` (0052), `sites.last_used_at` and the
`site_export_links_used` table (0053). Run `simple-host migrate`
before the new image. Skills are at 0.15.1 (0.11.0 still works).

### Sites
- Preview before live. An update sent with `?publish=false` (MCP
  `deploy_site` with `publish: false`) stores the new version without
  changing what visitors see; the answer's `new_version` is its number, and
  it is audited as `site_update` with `published: false`. A create refuses
  it (`publish_required`). The owner or a team member opens any kept version
  through a private link that lasts an hour (`GET .../versions/{version}/preview`,
  MCP `preview_version`, "Preview" in the Manage panel's Versions list):
  it still needs their own sign-in, nobody else can open it whatever the
  site's access level, it is never indexed or cached, and saves from it are
  refused (`preview_read_only`) when the browser reports the preview page as
  the Referer (a page that suppresses its Referer can still save to the live
  site's data). "Make live" (and `rollback_site`) publishes
  it. Version lists now say which version is `live`.
- Download a whole site: the owner or a team member gets a single-use,
  10-minute download address (a second use answers 410; the request log
  never records it) (`POST .../export-link`, MCP `export_site`, "Download
  site" in the dashboard's Manage panel) for one zip of the live files, the
  saved data and its history, the version list and the uploaded files. The
  address needs no sign-in; the person who asked must still own the site or
  be in its team when it is opened. Audited as `site_export`.
- Shared with me: `GET /api/collaboration/sites?include=shared` and MCP
  `list_sites` add the sites shared with the caller by name or through a
  team, as `access_role: "viewer"` entries with `shared_via`.

### Agents
- `list_site_assets` and `delete_site_asset`: list and delete a site's
  uploaded files from a chat app (delete asks first). The storage-quota hint
  now points at them.
- `site_activity`: one site's versions (who published each, and which is
  live), its recent recorded changes with who made them (`actor_name`, the
  same rule as the dashboard's Activity), and its visit counts, in one read.
  A part the credential cannot read becomes a sentence.
  `GET /api/access?summary=counts` returns the counts shape under
  `ACCESS_LOG_VISIBILITY=owner` too.

### API keys
- A new key keeps its own last four characters; the dashboard shows
  "ends …abcd" (older keys: "earlier key"), when it was last used, and
  "expires soon" in its last 14 days.
- A refused key says why: `key_expired` (with the date), `key_revoked` or
  `key_not_recognised` (also the answer for a disabled person's key, so a
  found key does not reveal that its owner left). A key with 14 days or less
  left gets `X-Key-Expires` and `X-Simple-Host-Notice` on every response.
- Admins revoke a leaked key by pasting it on /admin ("Revoke a leaked
  key", `POST /api/admin/keys/revoke`): only that key stops working, and the
  audit row (`admin_key_revoke`) names its owner.

### Admin page
- "This instance" on /admin: release, commit and schema, migrations waiting,
  this replica's latest bucket check, owner certificates ready and waiting
  (each waiting owner with how long), and the limits in force.
- New metric `simplehost_owner_hosts_not_ready`.

### Recovery
- `simple-host verify-storage` lists every live version and uploaded file
  the database depends on that is missing from the bucket (by key) and exits
  non-zero: run it after a database point-in-time restore.
- The restore drill is documented (`docs/install.md`, "Restore drill": a
  site with `restore`, and the database by PITR to a new instance), tested
  end to end against a real database, and run by `make smoke`.

### Idle sites (opt-in)
- `IDLE_CLEANUP_DAYS` (off by default): a site nobody has opened (its owner
  and team count; bots and previews do not), deployed to, or read or written
  saved data on for that many days (`sites.last_used_at`, written at most
  hourly per site) is marked. Sites that existed before 0053 count as used
  when it ran, so none is marked until `IDLE_CLEANUP_DAYS` after the
  upgrade; its owner or team sees "Not used
  lately" on the dashboard with the date, Keep and Download, admins see the
  list on /admin, and with `SMTP_URL`/`SMTP_FROM` the owner (or each team
  member) is emailed (STARTTLS required on `smtp://` unless the URL ends in
  `?insecure=1`; a display-name `SMTP_FROM` works). 30 days later, still unused and not kept, it moves to
  Recently deleted. Any use unmarks it; Keep (`POST .../keep`, MCP
  `keep_site`) takes it out for good. Every step is audited.

### Audit log
- /admin's Activity card searches by owner, site, person, action and date,
  shows names instead of ids, loads more, and its export links carry the
  same filters (`GET /api/admin/export` now takes `owner`, `site`, `actor`,
  `action`; CSV gains `actor_name`, `owner_name`, `site_name` at the end).
- `GET /api/audit` events carry `owner_name`, `site_name` and `actor_name`,
  and `from`/`to` also take a `YYYY-MM-DD` date. A site's Activity on the
  dashboard shows who made each change: owners and team members see the
  names of themselves and fellow members, never a visitor's.

### Access
- Opening a site you can't see now shows a page instead of a bare 404: "This
  site doesn't exist or isn't shared with you", who you are signed in as,
  and Switch account (sign out and back in as someone else, then return to
  the site). A missing site and one not shared with you look the same, and a
  signed-out browser is asked to sign in first for both; scripts and agents
  still get a plain 404.

### Search
- The `/showcase` search box also finds sites by the words on their pages
  (full-text search), not only by owner and site name; the name match still
  answers as you type and on its own if search is unavailable.
- MCP `search_sites`: the same full-text search for agents.

### Owner index
- A team's index page (`team-<name>.<base>/`) shows its members every team
  site, as the owner's page does for the owner; others still see only the
  listed ones.

### Dashboard
- Each site links to its address and shows its live version and last update.
- The Manage panel adds Versions (Preview and Make live), Saved data (history with
  Restore), Download site, and Delete (type the site name; it goes to
  Recently deleted).
- A "Shared with me" section lists the sites shared with you.
- A "Teams" section: create a team, see its members (people not signed in
  yet included), add and remove them, leave, and delete a team (type its
  name). Leaving as the last active member says how many sites go with the
  team and asks for the name first.
- With no sites yet, the list shows how to connect an AI app: the `/mcp`
  address, `plugin.zip` and the install page.

## v1.4.0 — 2026-09-27

Schema 0049 (from 0042). Run `simple-host migrate` before the new image.
0043 (`site_redirects`), 0045 (`pending_site_viewers`,
`pending_team_members`), 0046 (rewrites `audit_ensure_partitions`, creates
audit and access-log partitions twelve months ahead), 0047 (nullable
`sites.access_decision*` columns), 0048 (`erased_owner_labels`,
`erased_identities`, a trigger on `users`, `access_log_erase_visitor()`)
and 0049 (the definer-function security fix below) are backward-compatible. 0044
(`sites.deleted_at`, `sites.deleted_by`) is not: an older binary would serve
sites in their recovery window again, so it refuses to start on this
schema, and rolling back to v1.3.1 needs the database restored from before
the migration. Skills are at 0.14.1 (0.11.0 still works).

### Sites
- Move a site into a team: the owner moves their site into a team they are
  in, or a member moves a team's site into another team they are also in
  (`POST .../transfer`, MCP `transfer_site`, "Move to a team" in the site's
  Manage panel). Nobody hands a site to a person; an admin moves a leaver's
  sites to a team or a person. Versions, saved data and its history,
  uploaded files, the access level and viewers go with it; network access
  does not (the site drops to `company`, audited as
  `network_access_reverted`, and a pending request is withdrawn). Refused
  for anything but a team the caller is in (`destination_not_found`, one
  answer whatever the reason), while an admin's restriction stands
  (`site_restricted_by_admin`), when the team already has a site by that
  name (`name_conflict`), a recently deleted site of the team holds it
  (`name_held`), or it has no room (`site_limit`, `storage_quota`). Audited
  as `site_transfer` with `from` and `to`, in both namespaces' logs.
- Rename a site (`POST .../rename`, MCP `rename_site`, "Rename" in the
  Manage panel), keeping everything it holds, network access included.
  Refused while an admin's restriction stands.
- After either, the old address redirects to the new one (path and query
  kept) until a site takes the old name again, but only for somebody who
  can open the site there: anyone while it is open to the network,
  otherwise a caller signed in on the old address (a visitor with no
  session is taken through sign-in first) or carrying a key, whom the
  site's access admits. Everyone else, and everyone while the site is
  recently deleted, gets the ordinary not-found. The owner-hosts reconciler
  keeps the old owner's certificate while any of its addresses redirects.
- Deleting a site can be undone for 30 days. It stops serving and leaves
  every list at once, but keeps its versions, saved data and history, access
  level, viewers and uploaded files, and its name (a new site, a rename or a
  hand-over onto that name is refused with `409 name_held`; a deleted site
  itself cannot be renamed or handed over). The owner or a team member
  restores it from "Recently deleted" on the dashboard,
  `GET /api/deleted-sites` and `POST /api/sites/{site}/restore` (or the
  owner-qualified `/api/collaboration/sites/{owner}/{site}/restore`), or the
  new MCP tools `list_deleted_sites` and `restore_site`; an admin restores
  any from the "Recently deleted" card on `/admin`. A recently deleted site
  keeps counting toward its owner's site and storage quota until its window
  ends, so deleting does not make room at once. A restore is refused with
  `409 name_taken` if a live site now holds its address, and is audited as
  `site_restore`. After 30 days the site cannot be restored (even before the
  sweeper runs), and the sweeper removes it for good. Deleting a team still removes its sites at
  once.
- `simple-host restore` into a name a recently deleted site holds undeletes
  that site rather than creating an empty one.
- Leaving a team as its last active member, or deleting a team, now says the
  sites can be kept by moving them first.

### Sharing
- Site viewers and team members can be named by company email, including
  someone who has not signed in yet. The email adds the account that carries
  it; with no such account, it is kept as a pending grant, listed with
  `pending: true` ("hasn't signed in yet" on the dashboard), counted toward
  the 50 limit, and removable by that email. At the person's first sign-in
  with that verified email it becomes the real grant, audited as
  `pending_grant_converted`. Emails outside `ALLOWED_EMAIL_DOMAINS` are
  refused when that is set. Pending viewers follow the site through a
  rename or hand-over.
- Your dashboard (and `get_site`/`list_sites` as `access_decision`) shows
  the last admin decision about who can open a site: a network request
  declined or network access revoked, with the admin's note, until your
  next request; or a restriction, with its reason.

### People
- A newly created API key stays on the dashboard, with its paste-back
  block, until you press Done; the page no longer reloads and hides it
  before it can be copied. The new key joins the list in place.
- `/auth/sessions` lists your connected chat apps (name, connected, last
  used) with Disconnect, which ends that app's access at once
  (`GET /api/me/connections`, `DELETE /api/me/connections/{id}`, browser
  session only; audited as `connector_revoke`).
- "Sign out everywhere" on `/auth/sessions` ends every session of yours
  and, unless you clear the box, revokes your API keys and connected apps,
  in one audited transaction (`sign_out_everywhere`).
- Each sign-in refreshes a person's stored email from the address the IdP
  verified, so offboarding by email follows directory changes. An address
  another person already holds is not taken over (`email_change_skipped`).

### Admin
- Restrict any site to only its owner (or team) from `/admin` with a
  one-line reason: a take-down, audited as `site_restricted`. The owner sees
  the reason. The restriction is sticky: until an admin lifts it, the owner
  or team cannot raise the level, request network access or add viewers
  (`409 site_restricted_by_admin` with the reason; the dashboard's controls
  are off), nor rename it or move it into a team. Lift
  on `/admin` restores the earlier level (`company` for a site that was on
  the network).
- Declining a network request or taking a site off the network asks for
  an optional note for the owner, recorded in the audit event.
- A disabled person's row on `/admin` offers "Move to team…" (all their
  sites, to a team or a person, all or none; the only way a site reaches a
  person) and "Delete sites" (they go to
  Recently deleted, restorable for 30 days); a team with no active member
  offers "Move to team…" beside "Delete team". Nobody else's sites can be
  moved or deleted by an admin.
- Data subject requests: a disabled person's row on `/admin` offers
  "Export data" (`GET /api/admin/users/{username}/export`, one zip of their
  account, teams, viewer grants, key/connected-app/session metadata, every
  site with its live files, saved data and history, versions and assets,
  grants waiting for their email, their own visits and their audit events)
  and "Delete person and all data"
  (`POST /api/admin/users/{username}/erase`, username typed to confirm):
  sites skip Recently deleted and their objects are queued for the sweep;
  keys, connected apps, sessions, memberships, viewer grants, pending grants
  for their provider-vouched email and the access-log rows of their visits
  go too, and old addresses of sites they handed on stop redirecting (not
  found). A hand-over or team change touching them waits for it. Refused
  while they are a team's last member. Audit rows stay (hash chain) and
  keep the opaque id, and some their name or email in the detail, until
  retention prunes them; the new `user_erased` row names nobody. The name
  stays held so nobody inherits old links, and they cannot sign back in
  while the identity provider still admits them: sign-in says the account
  was erased by an administrator. `/admin`'s "Erased people" card allows
  sign-in again (`GET /api/admin/erased-identities`,
  `POST /api/admin/erased-identities/{id}/allow`). Audited as
  `admin_user_export`, `user_erased` and `erased_identity_allowed`.

### Security
- **Fix for every earlier release (upgrade and run `simple-host migrate`).**
  The database functions that run with the owning role's rights
  (`audit_bump_state_write` since 0027, `audit_chain_append` since 0036,
  `audit_chain_entry` since 0040) searched the session's temporary schema
  first. Whoever could run SQL as the application role (through SQL
  injection or a leaked `DB_APP_PASSWORD`) could use that to run SQL as the
  migration owner and rewrite the audit trail. Migration 0049 pins their
  search path and schema-qualifies what they name, and takes `TEMPORARY` on
  the database and `CREATE` on the `public` schema away from `PUBLIC`.
  Nothing else changes; no configuration is needed.
- A viewer or team member named by email must be a plain ASCII address
  (letters, digits and `._%+-` before the `@`, a dotted domain); quotes,
  brackets and non-ASCII look-alikes are refused. Every dashboard and
  `/admin` list escapes quotes as well as markup, and `/admin`'s confirm
  and prompt buttons carry their questions in data attributes, not inline
  script.
- A grant by email reaches an existing account only through an address the
  IdP verified at a sign-in, and a pending grant converts only when the
  verified `email` claim is plain ASCII and is the address the account now
  holds (not one another account keeps).
- Moves re-check team membership inside their transaction.
- Concurrent changes take their locks in one order (person or team row,
  then site names): moving a site out of a team no longer races the team's
  deletion (which could queue the moved site's files for removal), a bulk
  delete of a leaver's sites skips one an admin handed to someone else
  meanwhile, and a sign-in converting pending grants waits for a
  membership or viewer change instead of deadlocking with it.
- Migration 0046's partition functions are not executable by `PUBLIC`
  (only the owning role that runs `migrate` and `prune` uses them).

### Operations
- Startup warns when no admin is configured (neither `ADMIN_EMAILS` nor
  `OIDC_ADMIN_CLAIM`) and when the bucket's versioning is not enabled (a
  provider that cannot report it is logged, not flagged). Both are also
  exported as `simplehost_config_warning{check="no_admin"|"bucket_versioning"}`.
  The bucket credentials need `s3:GetBucketVersioning` for the check; without
  it the status is logged as unknown.
- `simple-host migrate-storage` and `simple-host reencrypt` record one
  audit event per run with their counts (`storage_migrate`,
  `storage_reencrypt`), like `restore`; dry runs are not recorded.
- Retention no longer wedges after a long gap: rows that reached the
  default partition while `prune` was not running are moved into their
  months' partitions, unchanged and still hash-chained, instead of every
  later run failing. The prune CronJob now runs daily (was monthly) and
  keeps partitions twelve months ahead (was two). A row lives for its
  retention setting plus up to one month. Creating a month's partition no
  longer deadlocks with a concurrent write.
- Docs state that the stdout request log carries full client IPs and user
  agents, so the log pipeline's retention governs those lines.

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
  (whose default admits the hosted AI apps) when the install is
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
