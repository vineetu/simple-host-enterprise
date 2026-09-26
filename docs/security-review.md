# Security review pack

This document maps the 21 weaknesses found in an audit of the code this
package was derived from to the control that closes each one here, lists
what a penetration test should try, states what has and has not been
tested, and states the accepted limitations once rather than scattering
them.

It is the author's own review, not an independent one. Section 3 says
exactly which tests have been run and which have not.

## 1. Threat model

**Assets.** Hosted site content and its per-site state document; uploaded
assets; the identity of every signed-in person and their session; API
keys; the action audit and access log; database and backup contents;
Kubernetes Secrets (OIDC client secret, session signing keys, database
credentials, backup envelope key); the cluster's TLS certificates.

**Actors.**

- An **employee** with an ordinary account, who may be a site's owner, a
  member of the team that owns it, a named viewer of it, or none of these.
- A **site author's** own hosted-content JavaScript, which is trusted to
  run on that site's origin but not on any other employee's.
- A **leaver**: someone whose employment or account ends while their sites
  and sessions still exist.
- A **cluster admin**: someone who can read Secrets, exec into the pod, or
  read the underlying volumes — a stronger position than any application
  role and explicitly out of the application's ability to defend against
  (see Accepted limitations).
- A **cloud/platform admin**: controls the storage class, the managed
  database, the bucket, and their key policies.
- A **network attacker**: can reach the base host and every owner host
  over the public or corporate network, but holds no valid session, key,
  or platform credential. Such a person can read a site an admin has
  approved for the network, and nothing else.

**Trust boundaries.** Browser to owner host; browser to base host; owner
host to base host (same process, different hostname, and the boundary the
whole subdomain-only design exists to hold — `docs/site-isolation.md`); pod to database;
pod to bucket; pod to OIDC provider; backup object at rest in the bucket;
volume at rest; Kubernetes Secret at rest.

## 2. Controls matrix

Findings are numbered S1–S21 as in that audit, grouped by the kind of fix:
**(a)** closed by SSO identity, **(b)** a configuration flip, **(c)** a
genuinely new build. "Built" means the control is in the code on this
branch.

### (a) Closed by SSO identity

| # | Finding | Control | Status |
|---|---|---|---|
| S1 | Plaintext API keys; the session cookie was the raw key | Keys hashed (SHA-256 of 256 random bits) and optional, minted from a session; the cookie carries a session id, not a key. Every key expires: 90 days by default, at most `API_KEY_MAX_DAYS` (365 at most) — `internal/handler/keys.go`, migration `0029_api_key_expiry.sql` | Built |
| S2 | Default admin key; admin was a synthetic principal with no row | Admin is an OIDC-identified person with a row; `ADMIN_API_KEY` removed entirely. The admin API accepts a browser session only, never an API key (`requireSessionAuth` in `internal/handler/admin.go`), and `ADMIN_EMAILS` is re-applied to every account at startup (`db.SyncAdminEmails`, called from `cmd/server/main.go`), so removing someone from the list takes effect on the next restart without their signing in | Built |
| S4 | Open registration; an account with no sites could hand its key to anyone who knew the email | Registration and key-recovery endpoints removed; accounts are created only by signing in | Built |
| S5 | State API unauthenticated; Referer attribution was forgeable | The viewer's host session names the person (or an `X-API-Key`); `Referer` is no longer read anywhere on this path — `internal/handler/site_api.go`, `host_gate.go`'s `serveSiteAPI`/`authenticateSiteAPI` | Built |
| S6 | No private sites; serving never consulted a visibility flag | Every site has an access level (`sites.access`, migration 0033): `only_me` (the default for a new site), `specific`, `company`, `listed`, `network`. Every view needs a signed-in host session except on a `network` site, which only an admin can approve. `ViewerAllowed` applies the level — `internal/db/site_viewers.go`, `internal/handler/host_gate.go`'s `checkSiteAccess`/`checkViewerAllowed` | Built |
| S7 | Hosted pages could share the control-plane origin until cutover | The cutover flag and the shared-origin path are deleted outright; only the subdomain shape exists, plus a dedicated hostname for a restricted site (`<owner>--<site>.<base>`) — `internal/handler/host_gate.go`, `host.go`'s `Classify`/`SiteURL`. The site-facing API follows the same rule: a restricted site's state and assets answer only on its own host and are 404 on the owner host (`serveSiteAPI` in `host_gate.go`; `TestHostGateSiteAPIRestrictedOnlyOnItsOwnHost`) | Built |
| S14 | Session cookies lacked `__Host-`; the admin cookie could not be revoked per browser | `__Host-` prefix on every session cookie (and on the search-session and per-site visit cookies); revocable session rows, one per sign-in. Every session cookie is bound to the host it was minted for and accepted only there: a base-host cookie carries no host and a hand-off cookie names its owner or restricted-site host (`internal/auth/session_cookie.go`'s `SignHostSession`). Hosted content checks it in `hostsession.go`'s `VerifyHostedSession`; `auth.Middleware` refuses a host-bound cookie on the base host and, for the site-facing API, requires the cookie's host to equal the requested host (`auth.WithExpectedSessionHost`, set by `host_gate.go`'s `serveSiteAPI`) on top of the session row's validity; the base-host pages that read the cookie themselves (`dashboard.go`, `admin.go`, `showcase.go`) use `auth.VerifyBaseSessionCookie`. Hand-off codes are stored as SHA-256 only and spent by the first redemption attempt whatever its outcome (`internal/db/handoff.go`), so a code delivered to a browser that cannot redeem it (the popup chain a page on its own owner host could otherwise run) cannot be carried off and redeemed with the attacker's nonce; service-worker script fetches are refused on hosted hosts so a restricted site's root-scoped worker cannot intercept `/auth/session` | Built |
| S15 | Rate limits keyed by load-balancer peer, coarse by design | A per-site limit is keyed by owner+site (`siteLimitKey`, `internal/handler/abuse_limits.go`); every per-caller limit is keyed by `clientLimitKey`: the signed-in user's id once a route has authenticated one (site-facing API, search, hand-off mint), otherwise the client address. The client address is `reqlog.ClientIP`: the TCP peer, or — when the peer is inside `TRUSTED_PROXY_CIDRS` — the right-most `X-Forwarded-For` entry that is not itself a trusted proxy. The same address is what the request log, `access_log.ip`, `sessions.ip` and audit rows record. Routes limited before authentication (sign-in, management and admin client limits) key by address. Sign-in, session hand-off, API key mint and the connector's token and registration limits are counted in Postgres (`rate_limit_counters`, hashed keys, `internal/ratelimit/shared.go`) and hold across replicas, falling back to the per-pod limit when the database does not answer; every other limit is per pod, so N replicas allow N times it (`docs/install.md`, "Rate limits and replicas") | Built |
| S17 | Email validation accepted any domain | The provider decides who signs in, and must vouch for the address: `email_verified` must be present and `true` (Entra ID, which never sends it, instead needs its documented `xms_edov` optional claim, accepted only from an Entra issuer), so an unverified address can neither match `ADMIN_EMAILS` nor bind an existing account by email. Entra's multi-tenant `/common`, `/organizations` and `/consumers` issuers are refused at startup. For Google with `ALLOWED_EMAIL_DOMAINS` set, the `hd` claim is required and must be an allowed domain, so a consumer account holding a company address is refused. Every refusal is audited as `sign_in_failed` with its reason (`internal/handler/auth.go`'s `refuseIdentity`) | Built |

### (b) Configuration flips

| # | Finding | Control | Status |
|---|---|---|---|
| S3 | Database DSN built with `sslmode=disable` | `verify-full` with a CA bundle required; weaker modes refused unless `DB_INSECURE_ALLOWED=true` for local evaluation | Built |
| S8 | No `Referrer-Policy`, no `Cache-Control`, `frame-ancestors` withheld on hosted content | `Referrer-Policy: strict-origin-when-cross-origin` on every host (`internal/handler/security.go`'s `SecurityHeaders`, unconditional as of the second review round); `Cache-Control: private, no-cache` on every owner/restricted-site-host response and `Cache-Control: no-store` on an authenticated base-host request; `Cross-Origin-Resource-Policy: same-origin` on every owner-host response — `host_gate.go`'s `applyOwnerHostSecurity`; a `Sec-Fetch-Site`/`Sec-Fetch-Dest` refusal on top for browsers that ignore CORP; framing stays an owner choice | Built |
| S10 | Pod lacked `readOnlyRootFilesystem`, dropped capabilities, `runAsNonRoot`, seccomp; helper held a writable binary mount | Hardened `securityContext` on every container; the binary-mount helper is gone entirely — the image carries the binary | Built |
| S11 | Unencrypted site volume, unencrypted snapshots, SSE-S3-only backups, shared RDS secret | Encryption at rest is a platform requirement the install guide states per cloud (`docs/cloud/`); the application applies SSE headers on every backup object and an optional client-side envelope on top | Built (app-side); platform-side is the installer's responsibility, documented, not enforced by the software |
| S12 | Installation-specific values compiled in as defaults | No defaults for anything that identifies an installation; startup lists what is missing | Built |
| S13 | Forwarders relayed a visitor bearer to an internal AI gateway, gated only by Referer | Removed outright; `docs/configuration.md` documents the extension point for a company that wants to add its own | Built (removed) |
| S16 | Reserved-label list built in, not configurable | `RESERVED_LABELS` config on top of the built-in set | Built |
| S18 | Extension denylist rather than allowlist for uploads | Kept deliberately for a site's own archive contents: uploads are served as bytes, never executed. A top-level `_assets/` archive entry is refused at extraction (`internal/tarball/extract.go`'s `isReservedAssetsPath`) so an upload cannot shadow the asset route. Uploaded assets themselves (`POST /api/sites/{site}/assets`) are a closed content-type allowlist over the sniffed bytes, never the declared type or extension (`internal/storage/assets.go`'s `classifyContentType`) | Built |
| S19 | AWS SDK modules several minor versions behind; `lib/pq` in maintenance mode | Dependencies pinned current; `govulncheck` clean and run in CI; `lib/pq` kept deliberately (supports `sslmode=verify-full` with `sslrootcert`, no driver change justified) | Built |
| S20 | Single database owner role; ownership enforced only in queries | Least-privilege `simplehost_app` role: `SELECT`/`INSERT`/`UPDATE`/`DELETE` on application tables only, no DDL, no `TRUNCATE`, and only `INSERT`/`SELECT` on `audit_events`; migrations run as the owning role from the init container only. The server connects as `DB_APP_USER` with `DB_APP_PASSWORD[_FILE]` (the owning role's `DB_PASSWORD[_FILE]` no longer decides its credential, and is blanked in the server container), and refuses to start if its role owns `audit_events`, can act as its owner, or holds `UPDATE`/`DELETE`/`TRUNCATE` on it (`migrate.CheckLeastPrivilege`) — so a `DB_DSN` naming the owning role is caught too. `DB_PASSWORD` equal to `DB_APP_PASSWORD` is refused by both `migrate` and the server. `audit_events.at` is set by a trigger from the database clock (migration 0030), and the coalesced `state_write` upsert only bumps its count | Built |
| S21 | HSTS omitted `includeSubDomains` | Added on the base host, now that the base is dedicated to this application — `internal/handler/security.go` | Built |

### (c) Genuinely new build

| # | Finding | Control | Status |
|---|---|---|---|
| S9 | No HTTP request log | Structured JSON request log on stdout, one line per request, with a generated id echoed as `X-Request-Id` | Built |
| S9a | No action audit beyond a few ad hoc tables | `audit_events` (partitioned by month, coalescing upsert for repeated state writes, least-privilege grant with no `UPDATE`/`DELETE` for the app role) is wired into every mutation handler — `site.go`, `collaboration.go`, `viewers.go`, `team.go`, `admin.go`, `site_api.go`, `assets_admin.go`, `admin_export.go`; `cmd/server/main.go`'s `auditRecorder` is a real `audit.NewDBRecorder(database)`. These actions call `RecordTx` inside a shared transaction, so the mutation cannot commit without its audit row: `site_create`/`site_update`/`site_delete`/`site_rollback` (`site.go`), `site_access`/`network_access_requested`/`network_access_reverted` (`access.go`), `network_access_approved`/`network_access_declined`/`network_access_reverted` from the admin page (`access.go`), `state_restore` (`access.go`), `viewer_grant`/`viewer_revoke` (`viewers.go`), `team_create`/`team_delete`/`member_add`/`member_remove` (`team.go`), and `state_write`/`asset_create`/`asset_delete` (`site_api.go`, plus `asset_delete`'s second call site in `assets_admin.go`'s dashboard mirror — both now share the guarantee; the asset file on disk stays outside the transaction by design in both, since an orphaned or already-removed file is recoverable while the row and its audit commit together). Ten actions remain best-effort `Record` with no shared transaction: `sign_in`/`sign_out`/`session_revoke` (`auth.go`), `key_mint`/`key_revoke` (`keys.go`), `hand_off` (`handoff.go`), and `admin_disable_user`/`admin_enable_user`/`admin_archive_versions`/`admin_export` (`admin.go`, `admin_export.go`) — see accepted limitations | Built |
| S9b | No access log; who viewed a site was never recorded | `access_log` and its batching `AccessWriter` (`internal/audit/access_writer.go`) are wired into `serveSite`'s status recorder (`serve.go`), enqueuing one row per hosted-content response including the owner's and team members' own. Under the default `ACCESS_LOG_VISIBILITY=counts` an owner or team member reading `GET /api/access` gets views per day and distinct viewers, never who; only admins see rows with names (`internal/handler/audit_access.go`'s `listAccessCounts`) | Built |

### (d) Hardening beyond the audit

Controls added on top of the 21 findings above.

| Control | What it closes | Status |
|---|---|---|
| Host-bound session cookie | A hosted-content or restricted-site session cookie's signed payload now carries the host it was minted for (`internal/auth/session_cookie.go`'s `SignHostSession`); `VerifyHostedSession` refuses a payload whose host claim does not match the request's own classified host, and refuses a payload with no host claim at all outright. This closes the gap `test/pentest`'s `TestSessionFixationAcrossHosts` demonstrated directly — a captured cookie value, presented by a non-browser client, would otherwise authenticate on every host the same person had ever opened, since the underlying session row is shared by design. | Built |
| Anonymous network access | A `network` site is open without sign-in only after an admin approves the owner's request (`/admin` "Access requests", `POST /api/admin/access-requests/{owner}/{site}/approve`); until then it keeps its level. With no valid host session, the host gate lets through only that site's pages and assets on its owner host and a `GET` of its saved data (`host_gate.go`'s `requireHostSessionOrNetwork`, and the anonymous branch of `serveSiteAPI`). It cannot write saved data, upload or delete assets, reach the management API, or open any other site. Every other owner-host protection (`nosniff`, CORP, no service workers) stays. The owner can drop the level at any time; an admin can revoke to `company`. Each step is audited | Built |
| Streaming archive size checks | `internal/tarball`'s extraction now checks each entry's own declared size against the remaining per-file/aggregate budget *before* allocating or decompressing it (`checkDeclaredSize`), so a refused entry costs zero allocation instead of up to the full 500 MiB cap; an accepted entry is read once at its own declared size rather than `io.ReadAll`'s doubling growth. A one-byte probe after the declared bytes catches a declared-small/actual-large decompression bomb at the cost of one byte read, not the hidden payload's size. Found and fixed after `TestArchiveBombs` OOM-killed the pod at the documented per-file ceiling on a 1Gi memory limit | Built |

## 3. What a penetration test should try

**What was run.** Every item below has a scripted test in `test/pentest`
(build tag `pentest`; that package's doc comment lists the three required
and two optional environment variables, and `make pentest` runs it). The
author ran the whole suite, one `-run` at a time, against a local overlay
(`make local`), plus the hand-off code expiry case by hand with `curl`.
The Result column records that run, which was made against the first
release's code. The later fixes cited in section 2 (verified email, key
expiry, the session-only admin API, trusted proxies, the database-clock
audit trigger, the restricted-site API rule, the hand-off code spent on any
attempt) are covered by unit tests that `make test` runs; the pen-test
suite has not been re-run as a whole against them.

**What was not run.** No independent or third-party penetration test has
been done. The suite has not been run against a cloud installation. The
browser-only checks at the end of this section have not been run: they
need a real browser, and the suite is a Go HTTP client.

| Item | How | Coverage | Result |
|---|---|---|---|
| Label spoofing | Attempt to register or claim a reserved or look-alike label | scripted: `test/pentest/label_spoofing_test.go` | Pass |
| Hand-off code replay | Redeem a hand-off code a second time, or from a different session | scripted: `test/pentest/handoff_replay_test.go` (the "redeem after the 60s window" case is skipped without `PENTEST_SESSION_IDLE` and a real wait — see that file) | Pass — scripted subtests pass; the 60s-expiry case run by hand separately, see below |
| Hand-off login CSRF without the nonce | Attempt the hand-off redirect without the nonce cookie the owner host set | scripted: `test/pentest/login_csrf_test.go` | Pass |
| `to` open-redirect shapes | Attempt every disallowed shape (`//`, `/\`, a scheme, a host) against the post-hand-off redirect target | scripted: `test/pentest/open_redirect_test.go`; `sanitizeRedirectPath` also covered by `internal/handler/auth_test.go`'s `TestSanitizeRedirectPath` table | Pass |
| Sibling-origin subresource loads of a restricted site | `<script src>`, `<link>`, `<img>`, `fetch` with `credentials: include` from another owner host, with and without `Sec-Fetch-*` headers | scripted: `test/pentest/sibling_origin_test.go` (the "with" cases); manual: a real browser's `<script src>`/`<img>` fetch against a restricted site is not exercised by a Go HTTP client and needs a hands-on check | Pass (scripted half); manual: not run — see "Browser-only checks" below |
| Asset content-type confusion | Upload content whose sniffed type disagrees with its extension or claimed type; confirm it is never served as `text/html` | scripted: `test/pentest/asset_content_type_test.go` | Pass |
| Path traversal in archives | Craft an archive entry with `../`, an absolute path, a backslash, or an empty path component | scripted: `test/pentest/path_traversal_test.go`; `internal/storage/keys_test.go`'s `TestKeysRejectNonCanonicalIDs` covers the bucket side: object keys are built only from validated ids (`docs/storage.md`) | Pass |
| State write across sites | Attempt to write one site's state while authenticated to a sibling site | scripted: `test/pentest/state_write_across_sites_test.go` | Pass |
| Origin bypass | Send a mutating request with a forged or missing `Origin` header | scripted: `test/pentest/origin_bypass_test.go` | Pass |
| Session fixation across hosts | Attempt to reuse a session artifact minted for one host on another | scripted: `test/pentest/session_fixation_test.go` (`TestSessionFixationAcrossHosts`) — this is the test that found the gap the host-bound cookie control above closes | Pass — the host-bound session cookie control (section 2(d)) closes this live; confirmed against the local overlay |
| Idle-session survival on hosted content | Confirm an idle session stops serving hosted content within the documented window with no per-request database read | scripted: `test/pentest/idle_session_test.go`, skipped without `PENTEST_SESSION_IDLE` (a fixed 60-second window otherwise, per that file) | Pass — run live with `PENTEST_SESSION_IDLE=90s` |
| Key hash timing | Compare response timing for a valid key prefix against an invalid one | scripted: `test/pentest/key_timing_test.go` | Pass (statistical; see the test's own caveat) — after a test-side pacing fix, see below |
| Export scope escape | Attempt to export another owner's audit or access rows through the admin export endpoint | scripted: `test/pentest/export_scope_test.go`, plus `scripts/smoke.sh`'s own owner-vs-stranger `GET /api/audit` check, now that the admin export route and the scoped read routes both exist | Pass |
| Archive bombs | Upload an archive that exceeds the documented per-file, per-site, or count limits | scripted: `test/pentest/archive_bombs_test.go` and `internal/tarball/bomb_test.go` (entry count, aggregate size, per-file size, path depth, path length) | Pass |
| Top-level `_assets/` upload | Upload an archive containing a top-level `_assets` file or directory, and a direct asset-path upload attempting the same shadow | scripted: `test/pentest/assets_reserved_path_test.go`; `internal/tarball/assets_reserved_test.go` | Pass |

**Pacing the suite.** `authClientPolicy` (keyed by client address, Burst 20/refill 0.2/s on `/auth/*`) genuinely exhausts
partway through a sequence of back-to-back fresh sign-ins, producing `429`s
or "gave no redirect to the identity provider" that read as failures but
are the limiter doing its job. Run the suite in several `go test`
invocations (or raise the local overlay's rate limit for that run only)
rather than read a wall of these as a regression. See `docs/install.md`
section 11.

**Browser-only checks (not run).** With a restricted site
`alice--secret.<base>` and an ordinary site on `alice.<base>`, signed in
as a named viewer of the restricted site, from a page on `alice.<base>`:

1. `fetch('https://alice--secret.<base>/api/site/state', {credentials: 'include'})`
   must fail (no readable response), as must `<script src>`, `<link>` and
   `<img>` pointing at the restricted host.
2. `fetch('/api/sites/secret/state')` on `alice.<base>` must return 404.
3. In the browser's developer tools, the `__Host-` session cookie set for
   `alice.<base>` must not be sent to `alice--secret.<base>` or the base
   host, and `document.cookie` must not show it (it is HttpOnly).

## 4. Accepted limitations

Stated once, not repeated per finding:

- **The running pod holds plaintext.** Whoever can exec into it, or read
  the platform's key policy for the volume and the database, reads content
  regardless of anything this application does. The optional
  envelope key moves the *bucket* out of that set of readers — a leaked
  stored object alone is not enough without the Kubernetes Secret that
  holds the envelope key — but it does not, and cannot, protect against
  someone who already controls the pod or the cluster's own secret store.
- **Same-owner sites share an origin.** A page for one of an owner's sites
  can name a sibling site of the same owner in its state or asset calls;
  this is allowed on purpose because same-owner sites already
  share a browser origin, and the audit record distinguishes the claim
  (`via_site`) from what the browser can corroborate (`via_site_observed`)
  rather than pretending to prevent something a shared origin cannot
  prevent. See `docs/site-isolation.md` for the recorded path (sandboxed pages with an injected site-bound token) should this ever need closing.
- **Platform-side encryption at rest is a requirement stated to the
  installer, not something the software enforces.** The application
  applies the two controls it can (SSE headers on stored objects, the
  optional client-side envelope); whether the underlying volume, snapshot,
  and managed database are actually encrypted depends on the installer
  following `docs/install.md` and `docs/cloud/`.
- **An accepted archive entry is held in memory up to the per-file cap.**
  `internal/tarball`'s streaming check (see "Hardening beyond the
  audit" above) refuses an oversized entry before
  allocating anything, but a *legitimate* entry at or near the documented
  500 MiB per-file ceiling is still read fully into memory before it is
  written out — extraction is not yet streamed to disk. The reference
  deployment's memory limit is 2Gi, accounting for one such upload plus the second concurrent
  upload slot `abuse_limits.go` allows, not two simultaneous worst-case
  uploads landing in the same GC window; that remains a capacity-planning
  question for an installation's real traffic, and a deeper fix (streaming
  extraction so the ceiling stops depending on RAM at all) is unstarted.
- **`/api/audit`'s `actor_id`/`owner_id`/`site_id` fields are not resolved
  back to usernames or site names.** The dashboard's Activity tab (and the
  admin-wide equivalent) shows raw ids for those three columns; the
  action, timestamp, and `via_site_label`/`via_site_name` (already
  human-readable on the row) carry most of the practical signal in the
  meantime. A fix needs either a join in `internal/db`'s query or a batch
  lookup in the handler.
- **Every admin action still audits with `Record`, not `RecordTx`.** A
  follow-up landed moving the site-facing API's `state_write`
  (`PutState`/`PutStateVersioned`), `asset_create`, and `asset_delete`
  (`internal/handler/site_api.go`), plus `asset_delete`'s second call site,
  the dashboard's base-host mirror (`internal/handler/assets_admin.go`'s
  `deleteCollaborationAsset`), onto `RecordTx` inside a transaction shared
  with the database write — confirmed directly:
  `grep -n "RecordTx(" internal/handler/site_api.go` hits all four call
  sites (`PutState`, `PutStateVersioned`, `CreateAsset`, `DeleteAsset`),
  and the same grep against `assets_admin.go` now hits
  `deleteCollaborationAsset` too. In both files, the asset's file on disk
  is deliberately kept outside the transaction: an orphaned or
  already-removed file from a partial failure is recoverable, while the
  row and its audit row now commit or roll back together. What did
  **not** move: `admin_disable_user`/`admin_enable_user`/
  `admin_archive_versions`/`admin_export`
  (`admin.go`, `admin_export.go`) still call the older, best-effort
  `Record`. `admin_disable_user`/`admin_enable_user` specifically cannot
  move without a signature change to `db.SetUserDisabled`, which opens and
  commits its own internal transaction with no way for the caller to
  share it; the other three admin actions are disk/worker operations with
  no database transaction of their own to share.
- **`audit_events` and `access_log` carry no foreign keys on `actor_id`,
  `key_id`, `owner_id`, `site_id`, `team_id`, `user_id`, or `session_id`**
  (migration `0028_audit_events_drop_fks.sql`, dropping constraints
  migration 0027 originally added). Deliberate, not an oversight: an audit
  trail is an append-only record of what happened, so it must outlive the
  sites, teams, keys, and users it names rather than block their deletion
  or have its own history nulled out from under it. The gap this closed —
  `DELETE /api/sites/{site}` always 500ing because `site.go`'s
  `deleteSiteForTarget` deletes the `sites` row before its `site_delete`
  audit insert, which then violated `audit_events_site_id_fkey` — is
  since fixed.
  A row that names a since-deleted subject keeps that subject's raw id
  forever; there is no join-time guarantee it still resolves to a live
  row, which is the same limitation the `actor_id`/`owner_id`/`site_id`
  "not resolved back to usernames" bullet above already describes for the
  live case.
- **There is no site-transfer feature and no per-site write-mode
  setting**, so there is nothing to audit for either. Every site's saved
  data is writable by anyone signed in who may open it (anonymous network
  visitors read only). The last 20 versions are kept, and the owner or
  team can restore one (`state_restore`).
- **`GET /api/admin/export` is mounted without the admin dashboard's
  Origin check** (`dashboardCheck` in `admin.go`), unlike every mutating
  admin route (`disable`/`enable`/`teams/{team}/delete`/`access-requests`),
  which all wrap it. Deliberate, not an oversight: the export is a
  side-effect-free `GET`, and this package sends no
  `Access-Control-Allow-Origin` header anywhere (confirmed:
  `grep -rn "Access-Control-Allow-Origin" internal/handler/*.go` finds
  nothing), so a page on another origin can cause the browser to issue the
  request but cannot read the response body back — same-origin policy
  blocks that regardless of the Origin check. The asymmetry with the other
  admin routes is intentional, not a gap to close.
- **The browser's own same-origin cookie-jar behavior is relied upon, not
  tested by this suite.** `TestSessionFixationAcrossHosts`
  (`test/pentest/session_fixation_test.go`) proves the *server's* defense
  in depth: presenting one host's session cookie value directly to another
  host is refused because the signed payload now carries the host it was
  minted for (section 2(d) above). It deliberately does this outside a
  browser, with a bare HTTP client, precisely because a real browser would
  never send an owner host's `__Host-`-prefixed cookie to a different host
  in the first place — that half of the guarantee is standard browser
  behavior this package depends on, and it has not been observed in a
  real browser. See "Browser-only checks" in section 3 for the steps.

## 5. What this review did not cover

- An independent penetration test. Section 3's run is the author's own,
  against a local cluster, and leaves the browser-only checks unrun.
- Load-balancer and cluster configuration outside this repository.
- The skill and MCP client code paths beyond routing (`internal/mcp/*_test.go`
  proves every tool's path resolves against the real router, not that the
  client behaves correctly with hostile input).
