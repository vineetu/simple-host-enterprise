---
name: simple-host
description: Deploy and collaborate on static websites in Simple Host. Use when an agent needs to get the user signed in and holding an API key, validate and package a local site, choose the namespace a site is published under, create or update a site, resolve owned/shared/team sites, download an exact retained artifact, deploy with ETag conflict protection, create a team or manage its members, manage editors, list versions, roll back, or delete.
---

# Simple Host

Simple Host serves static files from a namespace owned by a person or a team,
at `/sites/{owner_username}/{sitename}/` and at `{owner-label}.{base}/{sitename}/`.
It does not execute uploaded server code. Metadata lives in Postgres; deployed
files are retained as immutable versions on server-managed storage.

## 1. Check the skill version first

This skill is version `{{VERSION}}`. Before any other action, request:

```http
GET https://simple-host.example.com/skills/version
```

The manifest retains `version` and `sha256` for older clients and also describes
the bundle, minimum supported version, byte size, release type, reload
requirement, and release-notes URL.

- If `version` is `{{VERSION}}`, continue.
- If the manifest's semantic version is newer, read
  [`references/updating.md`](references/updating.md) completely. Tell the user
  the installed and available versions and ask permission before installing.
  Never update silently, including patch updates.
- If the installed semantic version is newer than the server manifest, do not
  downgrade. Continue with the installed skill and note that the server appears
  to be behind or rolling out.
- If the check is unreachable, continue with the installed skill. A failed
  version check alone must not block the user's task.
- If a downloaded bundle fails SHA-256 or safety validation, do not install it;
  report the integrity failure briefly and continue only where the current
  instructions remain accepted by the server.

After a verified update, re-read the active on-disk `simple-host/SKILL.md`
completely and every reference required by the pending operation before doing
anything else. If this runtime cannot safely re-read the updated instructions,
stop and ask the user to invoke the task again. Never continue the operation
with the pre-update instructions still in context.

Send these headers on every authenticated management request:

```http
X-API-Key: <api_key>
X-Skill-Version: {{VERSION}}
```

If the server returns `400` with `code: "skill_version_required"`, stop the
management operation, follow the permissioned update flow, reload the installed
instructions, and then restart the operation from access resolution.

## 2. Resolve access before answering about an existing site

For any question or action involving viewing, editing, updating, downloading,
rolling back, deleting, or sharing an existing site, first call:

```http
GET /api/collaboration/sites
X-API-Key: <api_key>
X-Skill-Version: {{VERSION}}
```

Never use legacy `GET /api/sites` to determine editability. For normal users it
lists owned sites only, and omits both sites shared with them and sites owned by
their teams.

Resolve and retain the exact `(owner_username, name)` pair. Report these facts
separately when relevant:

- **Publicly viewable:** the public URL loads; this alone says nothing about
  authenticated access.
- **Authenticated access:** the canonical pair appears in
  `/api/collaboration/sites`.
- **Editable:** `access_role` is `owner`, `member`, or `editor`. All three may
  deploy, download the artifact, list versions, and roll back.
- **Namespace-level:** `owner` and `member` may additionally create a site in
  that namespace, change its listing, delete it, and manage its editors. An
  `editor` may do none of those four.

`member` means the owner is a team and you are in it. A team is a namespace that
owns sites exactly as a person does, but it is not a person: it has no API key,
ever, and you always act with your own personal key. Before any team operation
read [`references/teams.md`](references/teams.md) completely.

A shared or team-owned site remains one owner resource, one URL, one version
history, and one stored copy. Never create an alias or copy under the acting
user's own username.

Before any existing-site operation, read
[`references/collaboration.md`](references/collaboration.md) completely. It is
the source of truth for owner-qualified routes, retained ETags, exact artifact
downloads, conflict handling, rollback, and editor management.

## 3. Choose the namespace before you build

A site's owner is either you or a team you belong to. Settle which **before
anything is built**: the owner's name is part of the base path, and the base
path is baked in at build time. Deciding at upload ships a bundle compiled for
the wrong path.

A project records its namespace in `simple-host.json` — visible, committed, one
per packaging root (the directory that is uploaded, or whose build output is
uploaded). Never at a monorepo root.

```json
{"schema": 1, "owner": "acme-team",
 "owner_id": "1168eebb-f107-4de3-b040-224498d4df0f",
 "site": "dashboard"}
```

It binds the project to a **namespace**, not to a site row, and it deliberately
records **no base path**; see the rule below for why.

- **Marker present:** call `GET /api/me`, which returns
  `{id, username, is_admin, kind, email, teams: [{id, name}]}`. Require the
  marker's `owner_id` to match either that `id` or one of `teams[].id`, and the
  marker's `owner` to be the matching name. A different ID means the name has
  changed hands since this project was bound: stop and say so. Never rewrite the
  marker and never fall back to
  creating the site under yourself. If you are not in that namespace, stop and
  say the project publishes to an address your account cannot act on; ask to be
  added rather than publishing a copy elsewhere. Then use
  `GET /api/collaboration/sites` to see whether the site exists: if it does,
  update it; if it does not, say so and create it under that name.
- **No marker, site the user names:** match on name in
  `GET /api/collaboration/sites`. One match: use it, and write the marker.
  Several: show the full URLs and ask. Never tie-break on role, and never read
  "my" as "personal".
- **No marker, new site:** call `GET /api/me`. No teams: personal, no question.
  Otherwise ask once which namespace, showing the resulting URLs rather than the
  words "personal" and "team", with personal as the default. Ask before
  building.
- Write the marker at the packaging root before building, and exclude it from
  the archive. Never create a team as a side effect of a deploy.
- A copied, forked, or templated project carries the marker with it. "New",
  "fork" and "template" never silently reuse an existing binding: say which site
  the marker points at and ask first.

### Compose to build, quote to report

The base host never serves site content or a site-facing API; every site is
served on its owner's own address, `<owner-label>.<base>/<sitename>/`, where
the owner label is the owner's name with dots turned into hyphens. A site an
owner has restricted to named viewers moves to its own flat address instead,
`<owner-label>--<sitename-label>.<base>/` with no `/<sitename>/` segment —
the whole host is that one site. Only the platform decides whether a site is
restricted; never guess it, and never compose either address yourself.

- **To build, compose the root path `/`.** A framework's base-path setting
  should be the site root (`/`), never `/<sitename>/`: a site's own base path
  changes shape the moment somebody restricts it, from a subpath on a shared
  host to the root of its own host, and a build wired to the wrong shape
  breaks silently (absolute asset URLs 404). Building against the root is the
  one base path that is correct in both cases.
- **To report, quote.** Always give `url` and `public_path` exactly as the API
  returned them. Never show or assemble an address yourself: which of the two
  forms above applies, and its exact spelling, is the platform's decision.
- **A page must never compute its own address from `location.pathname`.**
  On the restricted-site form there is no `/<sitename>/` segment to read one
  from in the first place.

## Service

- Base URL: `https://simple-host.example.com`
- API reference: `/docs.html`
- Install/update page: `/install.html`
- Public showcase and search: `/showcase`
- Config: `$HOME/.simple-host/config.json` on macOS/Linux or
  `$HOME\.simple-host\config.json` on Windows

## Read the reference that matches the operation

References are one level deep. Read each selected file completely before acting.

| Operation | Required reference |
|---|---|
| Install or update skills | [`references/updating.md`](references/updating.md) |
| Sign in and get a key | [`references/account-recovery.md`](references/account-recovery.md) |
| Edit, download, roll back, delete, or share an existing site | [`references/collaboration.md`](references/collaboration.md) |
| Create, list, join, leave, or delete a team | [`references/teams.md`](references/teams.md) |
| Detect and build a framework for subpath hosting | [`references/frameworks.md`](references/frameworks.md) |
| Validate, package, upload, and verify a site | [`references/packaging-and-validation.md`](references/packaging-and-validation.md) |
| Add state, search, or AI/browser capabilities | [`references/state-and-ai.md`](references/state-and-ai.md) |

Typical combinations:

- New framework site: account recovery if needed, then frameworks, then
  packaging and validation.
- New plain-HTML site: account recovery if needed, invoke
  `fix-paths-for-subpath-hosting`, then packaging and validation.
- Existing owned/shared/team site: collaboration first, then frameworks and/or
  packaging as required by the downloaded or local source.
- Anything naming a team: teams, then collaboration.

## Canonical deployment workflow

1. Load the saved `api_key` and `username`; if absent, follow account recovery.
2. Settle the namespace by section 3. For an existing site, resolve the canonical
   owner and role before inspecting or changing anything. Never assume the
   authenticated username is the owner.
3. Choose a safe site name. Prefer lowercase letters, numbers, and hyphens.
4. Detect the framework and build with the site root (`/`) as its base path —
   see "Compose to build, quote to report" for why a subpath base breaks the
   moment the site is later restricted. Simple Host cannot run SSR or a server
   process.
5. Validate the final static output, not the source tree.
6. Package files at the archive root; do not add an extra directory wrapper.
   Exclude `simple-host.json`.
7. Deploy, naming the intent you settled in step 2. Create and update are
   separate intentions, and a create that finds the site already there is an
   error — never retry it as an update.
   - **Create:** `POST /api/collaboration/sites/<owner>/<sitename>`, or the
     `deploy_site` tool with `owner` and `intent: "create"`. No ETag.
   - **Update:** the owner-qualified `PUT` with the ETag retained before
     editing, or `deploy_site` with `owner`, `intent: "update"`, and that same
     ETag.
8. Verify the site at the `url` the deploy response returns, quoted as
   returned, including its asset requests.

Do not upload source trees for projects with build systems. Do not string-rewrite
a framework bundle to repair its base path; rebuild with framework-native
configuration.

## Core collaboration and conflict rules

- Settle create versus update before you deploy, and say which you mean: an
  `intent` of `create` or `update` on `deploy_site`, or the matching `POST` or
  `PUT`. Nothing converts one intent into the other on a failure.
- Capture the current ETag before editing and send that retained value in
  `If-Match` for deploys and rollbacks.
- `412 Precondition Failed` means another actor changed the site. Stop, show the
  conflict, and reconcile intentionally. Never fetch a fresh ETag immediately
  before upload merely to force stale work through.
- `428 Precondition Required` on an owner-inferred legacy update means switch to
  the collaboration workflow.
- The downloadable ZIP is the exact retained deployed artifact, not reconstructed
  framework source. Do not claim otherwise.
- Keep deployed HTML, CSS, and JavaScript readable and stable where the toolchain
  permits. Do not deliberately minify or obfuscate unless the human asks.
- Editors and team members may deploy browser JavaScript and are trusted
  collaborators in the current shared-origin architecture. An owner or a team
  member may grant and revoke editors, change the listing, and delete the site;
  an editor may not.

## Platform rules that apply everywhere

- Information classification: only content the company allows to be shared
  internally, and content that is already public, may be published. Never
  publish confidential material, personal data, or customer data. Any signed-in
  person in the company can view an unrestricted site, so before a first publish
  tell the user this rule plainly; if what they
  asked to host looks like it crosses that line, stop and say so rather than
  publishing it.
- Static files only: no PHP, Node/Python/Go server, SSR runtime, or uploaded code
  execution.
- Archive limits: 100 MiB compressed request, 500 MiB per regular file and in
  aggregate after extraction, at most 50,000 entries, 32 path components, 1,024
  bytes per path, and 255 bytes per component. Symlinks and special files fail.
- The extension denylist is case-insensitive; it is not an allowlist. ZIP and DMG
  downloads inside a site are valid regular files.
- The server retains the latest five site versions.
- Viewing any site requires signing in; there is no anonymous viewing.
  Opening a site's address while signed out redirects through sign-in and
  back. Sites default to unlisted regardless: `/showcase` and public search
  include only sites set to `public=true`, and a restricted site (one with
  named viewers) is never public no matter what that flag says. Search returns
  at most one active HTML result per site.
- Site URLs: the deploy response and `GET /api/collaboration/sites` return each
  site's `url` and `public_path`. Quote those values when verifying or
  reporting a site; never show or assemble an address yourself. See "Compose
  to build, quote to report".
- Shared state and uploaded assets require the viewer's own signed-in session
  (or an agent's `X-API-Key`); there is no anonymous or Referer-based path.
  Never store secrets or PII in either. Use versioned state by default; use
  plain last-write-wins state only when the user explicitly requests it. A
  page calls `/api/sites/{site}/state/versioned` (or `/api/sites/{site}/state`,
  or the asset routes at `/api/sites/{site}/assets`), computing `{site}` from
  its own address with `location.pathname.split('/')[1]` — correct on an
  owner host's short path, which is the only path that exists there — and
  reloading the page on a `401` rather than retrying. See
  `references/state-and-ai.md` for the full contract and code.
- Every owner is served on their own address, and a restricted site moves to
  its own dedicated address the moment it gains a named viewer; a page cannot
  read or write another owner's state, assets, or hosted content. Sites
  belonging to the same owner still share that owner's origin on purpose.
- On `429`, read integer seconds from `Retry-After`, wait at least that long, and
  retry once per interval. Never hot-loop.

## Completion standard

Do not report success from an upload response alone. Confirm the `url` the API
returned loads, the entrypoint renders, and root-hosted asset paths are not
producing 404s. Report that URL and any remaining manual verification clearly.

**Send `X-Simple-Host-Check: 1` on every request you make while verifying.**
These checks are how the owner's own view counter used to get inflated: each
one delivered a page nobody read, and because a bare fetch stores no cookie,
each also registered as a brand-new visitor. The header tells the server this
request is a deploy check, so it is recorded as agent traffic and kept out of
the site's readership figures. It costs nothing and it is the difference
between a view count that means something and one that counts you.

Send it only for your own verification. Never send it when fetching a page on
behalf of the human, who is a real reader.
