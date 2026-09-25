# Security review pack

This is the document design.md section 12 asks the package to ship. It
maps every finding from the source-instance audit (design 4.1) to the
control that closes it, states which phase built that control, lists what
a penetration test should try against the finished package, and states the
accepted limitations once rather than scattering them.

Phases 0 through 5 are built and live-verified on the local overlay,
including a second review pass over Phase 2's own diff that found and
fixed four gaps (the CORP/`Cache-Control` headers, the idle-session touch,
and `Referrer-Policy` on the base host — see the controls table below) and
a live-wiring pass over Phase 4 that found and fixed two more (an audit row
that silently failed to insert for every site-facing API mutation, and a
prune CronJob pointed at a TLS secret that did not exist — see S9a/S9b
below). Phase 7 ran every pen-test item live against the local overlay and
filled in the Result column below; the Result column below
records what each one did.

## 1. Threat model

**Assets.** Hosted site content and its per-site state document; uploaded
assets; the identity of every signed-in person and their session; API
keys; the action audit and access log; database and backup contents;
Kubernetes Secrets (OIDC client secret, session signing keys, database
credentials, backup envelope key); the cluster's TLS certificates.

**Actors.**

- An **employee** with an ordinary account, who may be a site's owner, an
  editor on someone else's site, or neither.
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
  or platform credential.

**Trust boundaries.** Browser to owner host; browser to base host; owner
host to base host (same process, different hostname, and the boundary the
whole subdomain-only design exists to hold — design 7); pod to database;
pod to bucket; pod to OIDC provider; backup object at rest in the bucket;
volume at rest; Kubernetes Secret at rest.

## 2. Controls matrix

Findings are numbered as in design 4.1, grouped the same way: **(a)**
closed by SSO identity, **(b)** a configuration flip, **(c)** a genuinely
new build. "Phase" names where the control is implemented; "built" or
"pending" reflects this draft's date, not the finished package.

### (a) Closed by SSO identity

| # | Finding | Control | Design ref | Phase | Status |
|---|---|---|---|---|---|
| S1 | Plaintext API keys; the session cookie was the raw key | Keys hashed (SHA-256 of 256 random bits) and optional, minted from a session; the cookie carries a session id, not a key | 6.3 | 1 | Built |
| S2 | Default admin key; admin was a synthetic principal with no row | Admin is an OIDC-identified person with a row; `ADMIN_API_KEY` removed entirely | 6.2 | 1 | Built |
| S4 | Open registration; an account with no sites could hand its key to anyone who knew the email | Registration and key-recovery endpoints removed; accounts are created only by signing in | 6.1 | 1 | Built |
| S5 | State API unauthenticated; Referer attribution was forgeable | The viewer's host session names the person (or an `X-API-Key`); `Referer` is no longer read anywhere on this path — `internal/handler/site_api.go`, `host_gate.go`'s `serveSiteAPI`/`authenticateSiteAPI` | 7.3 | 3 | Built |
| S6 | No private sites; serving never consulted a visibility flag | Every view requires a signed-in host session; a site may list named viewers, enforced by `viewerAllowed` — `internal/db/site_viewers.go`'s `ViewerAllowed`, `internal/handler/host_gate.go`'s `checkSiteAccess`/`checkViewerAllowed` | 7.2 | 2 | Built |
| S7 | Hosted pages could share the control-plane origin until cutover | The cutover flag and the shared-origin path are deleted outright; only the subdomain shape exists, plus a dedicated hostname for a restricted site (`<owner>--<site>.<base>`) — `internal/handler/host_gate.go`, `host.go`'s `Classify`/`SiteURL` | 7.1, 5.2a | 2 | Built |
| S14 | Session cookies lacked `__Host-`; the admin cookie could not be revoked per browser | `__Host-` prefix on every session cookie; revocable session rows, one per sign-in; a host session's signed payload is additionally bound to the host it was minted for, so a captured cookie cannot be replayed on a different host of the same or another owner — `internal/auth/session_cookie.go`'s `SignHostSession`, `hostsession.go`'s `VerifyHostedSession` | 6.1 | 1 (base cookie), 2 (host-binding, a rule-5.6 hardening beyond the design's literal wording) | Built |
| S15 | Rate limits keyed by load-balancer peer, coarse by design | Partial: a per-site limit is keyed by owner+site (`siteLimitKey`, `internal/handler/abuse_limits.go`) and every base-host management route keys its per-caller limit by `user.ID` (`collaboration.go`, `keys.go`, `admin.go`'s `adminIdentityPolicy`). Design 7.5's remaining dimensions — session id for hosted content and the site-facing API's per-caller limit, key id for key-authenticated routes — are not built: `site_api.go`'s `stateClientPolicy`/`stateReadClientPolicy` and `handoff.go`'s `authClientPolicy` still key on `remoteClientKey` (peer address) | 7.5 | 2/3 | Built (partial) |
| S17 | Email validation accepted any domain | The provider decides who signs in; `ALLOWED_EMAIL_DOMAINS` kept for a multi-tenant provider | 6.1 | 1 | Built |

### (b) Configuration flips

| # | Finding | Control | Design ref | Phase | Status |
|---|---|---|---|---|---|
| S3 | Database DSN built with `sslmode=disable` | `verify-full` with a CA bundle required; weaker modes refused unless `DB_INSECURE_ALLOWED=true` for local evaluation | 9.2 | 5 | Built |
| S8 | No `Referrer-Policy`, no `Cache-Control`, `frame-ancestors` withheld on hosted content | `Referrer-Policy: strict-origin-when-cross-origin` on every host (`internal/handler/security.go`'s `SecurityHeaders`, unconditional as of the second review round); `Cache-Control: private, no-cache` on every owner/restricted-site-host response and `Cache-Control: no-store` on an authenticated base-host request; `Cross-Origin-Resource-Policy: same-origin` on every owner-host response — `host_gate.go`'s `applyOwnerHostSecurity`; a `Sec-Fetch-Site`/`Sec-Fetch-Dest` refusal on top for browsers that ignore CORP; framing stays an owner choice | 7.4 | 2 | Built |
| S10 | Pod lacked `readOnlyRootFilesystem`, dropped capabilities, `runAsNonRoot`, seccomp; helper held a writable binary mount | Hardened `securityContext` on every container; the binary-mount helper is gone entirely — the image carries the binary | 10.2 | 0 | Built |
| S11 | Unencrypted site volume, unencrypted snapshots, SSE-S3-only backups, shared RDS secret | Encryption at rest is a platform requirement the install guide states per cloud (`docs/cloud/`); the application applies SSE headers on every backup object and an optional client-side envelope on top | 9.1 | 5 (app-side) / installer (platform-side) | Built (app-side); platform-side is the installer's responsibility, documented, not enforced by the software |
| S12 | Installation-specific values compiled in as defaults | No defaults for anything that identifies an installation; startup lists what is missing | 10.1 | 0 | Built |
| S13 | Forwarders relayed a visitor bearer to an internal AI gateway, gated only by Referer | Removed outright; `docs/` (design 11) documents the extension point for a company that wants to add its own | 11 | 0 | Built (removed) |
| S16 | Reserved-label list built in, not configurable | `RESERVED_LABELS` config on top of the built-in set | 7.1 | 0 | Built |
| S18 | Extension denylist rather than allowlist for uploads | Kept deliberately for a site's own archive contents: uploads are served as bytes, never executed. A top-level `_assets/` archive entry is refused at extraction (`internal/tarball/extract.go`'s `isReservedAssetsPath`) so an upload cannot shadow the asset route. Uploaded assets themselves (`POST /api/sites/{site}/assets`) are a closed content-type allowlist over the sniffed bytes, never the declared type or extension (`internal/storage/assets.go`'s `classifyContentType`) | 7.3 | 3 | Built |
| S19 | AWS SDK modules several minor versions behind; `lib/pq` in maintenance mode | Dependencies pinned current; `govulncheck` clean and run in CI; `lib/pq` kept deliberately (supports `sslmode=verify-full` with `sslrootcert`, no driver change justified) | 10.4 | 0 | Built |
| S20 | Single database owner role; ownership enforced only in queries | Least-privilege `simplehost_app` role: `SELECT`/`INSERT`/`UPDATE`/`DELETE` on application tables only, no DDL, no `TRUNCATE`; migrations run as the owning role from the init container only | 9.3 | 5 | Built |
| S21 | HSTS omitted `includeSubDomains` | Added on the base host, now that the base is dedicated to this application — `internal/handler/security.go` | 7.4 | 2 | Built |

### (c) Genuinely new build

| # | Finding | Control | Design ref | Phase | Status |
|---|---|---|---|---|---|
| S9 | No HTTP request log; load-balancer logs were layer 4 only | Structured JSON request log on stdout, one line per request, with a generated id echoed as `X-Request-Id` | 8.3 | 0 | Built |
| S9a | No action audit beyond a few ad hoc tables | `audit_events` (partitioned by month, coalescing upsert for repeated state writes, least-privilege grant with no `UPDATE`/`DELETE` for the app role) is wired into every mutation handler — `site.go`, `collaboration.go`, `viewers.go`, `team.go`, `admin.go`, `site_api.go`, `assets_admin.go`, `admin_export.go`; `cmd/server/main.go`'s `auditRecorder` is a real `audit.NewDBRecorder(database)`. Sixteen actions call `RecordTx` inside a shared transaction, so the mutation cannot commit without its audit row: `site_create`/`site_update`/`site_delete`/`site_rollback`/`site_visibility` (`site.go`), `editor_grant`/`editor_revoke` (`collaboration.go`), `viewer_grant`/`viewer_revoke` (`viewers.go`), `team_create`/`team_delete`/`member_add`/`member_remove` (`team.go`), and `state_write`/`asset_create`/`asset_delete` (`site_api.go`, plus `asset_delete`'s second call site in `assets_admin.go`'s dashboard mirror — both now share the guarantee; the asset file on disk stays outside the transaction by design in both, since an orphaned or already-removed file is recoverable while the row and its audit commit together). Eleven actions remain best-effort `Record` with no shared transaction: `sign_in`/`sign_out`/`session_revoke` (`auth.go`), `key_mint`/`key_revoke` (`keys.go`), `hand_off` (`handoff.go`), and `admin_disable_user`/`admin_enable_user`/`admin_archive_versions`/`admin_classify_sites`/`admin_export` (`admin.go`, `admin_export.go`) — see accepted limitations | 8.1 | 4 | Built |
| S9b | No access log; who viewed a site was never recorded | `access_log` and its batching `AccessWriter` (`internal/audit/access_writer.go`) are wired into `serveSite`'s status recorder (`serve.go`), enqueuing one row per hosted-content response including the owner's and editors' own | 8.2 | 4 | Built |

### (d) Hardening beyond the source-instance audit

Two controls the implementation added on its own, not tied to one of the
21 findings above — recorded here so a reviewer does not read them as
undocumented scope creep.

| Control | What it closes | Design ref | Phase | Status |
|---|---|---|---|---|
| Host-bound session cookie | A hosted-content or restricted-site session cookie's signed payload now carries the host it was minted for (`internal/auth/session_cookie.go`'s `SignHostSession`); `VerifyHostedSession` refuses a payload whose host claim does not match the request's own classified host, and refuses a payload with no host claim at all outright. Design 6.1's literal wording ("same signed payload") does not name the host; this closes the gap `test/pentest`'s `TestSessionFixationAcrossHosts` demonstrated directly — a captured cookie value, presented by a non-browser client, would otherwise authenticate on every host the same person had ever opened, since the underlying session row is shared by design. Decided by the user (a rule-5.6 deviation) during Phase 2 | 6.1 | 2 | Built |
| Streaming archive size checks | `internal/tarball`'s extraction now checks each entry's own declared size against the remaining per-file/aggregate budget *before* allocating or decompressing it (`checkDeclaredSize`), so a refused entry costs zero allocation instead of up to the full 500 MiB cap; an accepted entry is read once at its own declared size rather than `io.ReadAll`'s doubling growth. A one-byte probe after the declared bytes catches a declared-small/actual-large decompression bomb at the cost of one byte read, not the hidden payload's size. Found and fixed after `TestArchiveBombs` OOM-killed the pod at the documented per-file ceiling on a 1Gi memory limit | (design 2's own tarball limits, not a numbered finding) | 3 | Built |

## 3. What a penetration test should try

Design section 12's list. All fifteen items now have a scripted test in
`test/pentest` (build tag `pentest`; see that package's own doc comment for
the three required and two optional environment variables). Phase 7 ran
every item live, one `-run` at a time (paced — see the rate-limiting note
below) against a cold local overlay, plus the one fixed-duration case
(hand-off code expiry) by hand with `curl`; every Result below reflects
that live run, not an isolated-in-phase or a static read of the test.
`the Result column below` has the full run log, including two
collateral rate-limit failures that were not product bugs (one a pacing
artifact, one a test-side pacing bug fixed in `key_timing_test.go`) and one
finding the run confirmed already closed (the host-bound session cookie —
section 2(d) above).

| Item | How | Coverage | Result |
|---|---|---|---|
| Label spoofing | Attempt to register or claim a reserved or look-alike label | scripted: `test/pentest/label_spoofing_test.go` | Pass |
| Hand-off code replay | Redeem a hand-off code a second time, or from a different session | scripted: `test/pentest/handoff_replay_test.go` (the "redeem after the 60s window" case is skipped without `PENTEST_SESSION_IDLE` and a real wait — see that file) | Pass — scripted subtests pass; the 60s-expiry case run by hand separately, see below |
| Hand-off login CSRF without the nonce | Attempt the hand-off redirect without the nonce cookie the owner host set | scripted: `test/pentest/login_csrf_test.go` | Pass |
| `to` open-redirect shapes | Attempt every disallowed shape (`//`, `/\`, a scheme, a host) against the post-hand-off redirect target | scripted: `test/pentest/open_redirect_test.go`; `sanitizeRedirectPath` also covered by `internal/handler/auth_test.go`'s `TestSanitizeRedirectPath` table | Pass |
| Sibling-origin subresource loads of a restricted site | `<script src>`, `<link>`, `<img>`, `fetch` with `credentials: include` from another owner host, with and without `Sec-Fetch-*` headers | scripted: `test/pentest/sibling_origin_test.go` (the "with" cases); manual: a real browser's `<script src>`/`<img>` fetch against a restricted site is not exercised by a Go HTTP client and needs a hands-on check per design 10.6 | Pass (scripted half); manual: not run — see below for the exact browser steps |
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

**A rate-limiting artifact, not a security finding, found while running
several of the tests above together:** `authClientPolicy` (design 7.5,
peer-address-keyed, Burst 20/refill 0.2/s on `/auth/*`) genuinely exhausts
partway through a sequence of back-to-back fresh sign-ins, producing `429`s
or "gave no redirect to the identity provider" that read as failures but
are the limiter doing its job. Every test above was independently
confirmed passing in isolation; Phase 7 should pace the full suite (batch
it into several `go test` invocations, or raise the local overlay's rate
limit for that run only) rather than read a wall of these as a regression.
See `docs/install.md` section 11 and this document.

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
  this is allowed on purpose (design 7.3) because same-owner sites already
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
  source-instance audit" above) refuses an oversized entry before
  allocating anything, but a *legitimate* entry at or near the documented
  500 MiB per-file ceiling is still read fully into memory before it is
  written out — extraction is not yet streamed to disk. The reference
  deployment's memory limit is 2Gi (lowered from 3Gi in Phase 4's wiring
  pass, once `checkDeclaredSize` made an oversized entry refuse before
  allocating rather than after — the 3Gi bump had been masking that
  earlier bug), accounting for one such upload plus the second concurrent
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
  `admin_archive_versions`/`admin_classify_sites`/`admin_export`
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
  recorded in `the Result column below`'s "Site-delete fix".
  A row that names a since-deleted subject keeps that subject's raw id
  forever; there is no join-time guarantee it still resolves to a live
  row, which is the same limitation the `actor_id`/`owner_id`/`site_id`
  "not resolved back to usernames" bullet above already describes for the
  live case.
- **`admin_transfer_sites` and `site_write_mode`** are two actions
  design.md 8.1 names that have no route anywhere in this repository to
  audit: no phase has built a site-transfer feature or a per-site
  write-mode setting. Nothing for the audit sink to wire until one exists.
- **`GET /api/admin/export` is mounted without the admin dashboard's
  Origin check** (`dashboardCheck` in `admin.go`), unlike every mutating
  admin route (`disable`/`enable`/`classify-sites`),
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
  behavior this package depends on, and Phase 7 had no browser tool
  available to observe it directly (no `<script src>`/`<img>`/credentialed
  `fetch` from one owner host against a restricted sibling, no
  DevTools/`document.cookie` read confirming the absence). See
  `the Result column below`'s "Browser-only checks" for the
  exact manual steps this still needs.

## 5. What this review did not cover

Carried forward from design 4.2, unchanged by this package:

- A running penetration test — section 3 above is the list, not the
  result, until Phase 7.
- Load-balancer and cluster configuration outside this repository.
- The skill and MCP client code paths beyond routing (`internal/mcp/*_test.go`
  proves every tool's path resolves against the real router, not that the
  client behaves correctly with hostile input).
