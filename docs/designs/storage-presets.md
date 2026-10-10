# Site storage with access presets

> **Status:** designed and built 2026-10-10 (FEATURES.md section 20). The
> owner decided on 2026-10-10 that Enterprise gets the storage access presets
> once they were tested on hosted Simple Host. This document is how they fit
> Enterprise's architecture.

## The ask

Hosted Simple Host gives each site three kinds of storage (key-value
namespaces, SQLite databases, file buckets) and, since 2026-10-10, one access
model for all of them: four actions (read, add, edit, delete), each given one
"who" value, with seven named presets. Enterprise should offer the same model
and the same presets, so the same agent instructions and page recipes work on
both, without breaking any Enterprise decision:

- agents connect through MCP over the company's OIDC; no new kind of key;
- the bucket is the primary store for files, and pods are stateless;
- Postgres is the one database;
- access levels and named viewers stay as they are;
- every change is on record; every limit is an env setting.

## What Enterprise already has

- **One JSON document per site** (`sites.state`), which anyone who may open
  the site may overwrite, with the last 20 versions kept for the owner to
  restore (section 10).
- **Uploaded assets** (section 11): bucket objects under
  `sites/<site-id>/assets/<uuid>`, rows in `site_assets`, per-file and
  per-site limits, the owner's `QUOTA_MAX_BYTES`, the optional clamd scan,
  the content-type allowlist, and deletion through the retire sweep.
- **The site-facing API** on each site's own host
  (`/api/site/...`, `/api/sites/{site}/...`), behind the host gate: the
  company sign-in, the access level and named viewers, Origin checks on
  writes, read-only previews, read-only anonymous visitors on a
  network-approved site, rate limits shared across replicas.
- **The hash-chained audit log**, written in the same transaction as each
  change.

Nothing here needs a new service, a new store, or local disk.

## Where the data lives

| Kind | Hosted | Enterprise |
|---|---|---|
| Key-value | SQLite file per site | Postgres rows: `site_storage_kv` |
| Tables | one SQLite file per database | Postgres rows: `site_storage_rows`, one JSON document per row, columns declared by the owner |
| Files | files on local disk | bucket objects, through the asset code (`sites/<site-id>/assets/<uuid>`), indexed by `site_storage_files` |
| Settings | `site_storage_resources` | `site_storage_resources`, one row per resource, the matrix per table inside |

**Tables are rows, not per-site schemas.** A per-site Postgres schema or a
table per site would mean runtime DDL, identifiers built from user input,
grants for the least-privilege `simplehost_app` role, and migrations that
cannot see the tables. Instead every table of every site is rows in one
Postgres table, keyed by `(site_id, resource, table_name, id)`, with the
values in a `jsonb` column. The owner declares each table's columns and their
types; the server checks every value against them. Nothing a visitor or
owner sends ever becomes an SQL identifier, so there is nothing to escape.

What this gives up, compared with hosted SQLite: the owner has no raw SQL
(`query`, `execute`). Owners read, add, change and delete rows through the
same row routes with full rights, and filter by equality. PARITY.md records
it as `different on purpose`.

**Files are bucket objects** written through the same code as uploaded
assets: the same envelope encryption, SHA-256 check on read, malware scan,
content-type allowlist and retire sweep. A replaced file is a new object; the
old one is queued for the sweep in the same transaction as the row change, so
objects stay write-once. A deleted site's purge already retires its whole
`sites/<site-id>/` prefix. File bytes count toward the owner's
`QUOTA_MAX_BYTES`, like assets.

**Pods stay stateless.** Every read and write is Postgres or the bucket. Locks
are Postgres row locks or transaction advisory locks, so two replicas
serialise correctly.

## The model (identical to hosted)

Each KV namespace, each file bucket and each table of a database has a
matrix: `read`, `add`, `edit`, `delete`, each one of `nobody`, `owner`,
`own`, `signed-in`, `anyone`. A database carries a default matrix and each
table may set its own.

The same four rules refuse a matrix on save (`400 invalid_access` with the
rule's code), leaving the same 161 of 625:

| Rule | Refused |
|---|---|
| R1 `anonymous_change` | edit or delete `anyone` |
| R2 `add_own` | add `own` |
| R3 `own_needs_signed_in_add` | any action `own` while add is `anyone` |
| R4 `blind_change` | edit or delete wider than read |

The same seven presets:

| Preset | read | add | edit | delete |
|---|---|---|---|---|
| `public` | anyone | owner | owner | owner |
| `inbox` | owner | anyone | owner | owner |
| `wall` | anyone | signed-in | owner | own |
| `records` | own | signed-in | owner | owner |
| `personal` | own | signed-in | own | own |
| `board` | signed-in | signed-in | signed-in | owner |
| `private` | owner | owner | owner | owner |

`private` is the default. A preset with single-action overrides is stored as
`custom`, naming its base.

## Who is who on Enterprise

Every caller of the site-facing API has already passed the host gate: they
are signed in with the company's identity provider and may open the site at
its access level, or they are an anonymous visitor to a site an admin opened
to the network. The matrix decides only what they may do with the data
inside.

| Value | Hosted | Enterprise |
|---|---|---|
| `anyone` | anyone, signed in or not | anyone who may open the site. On a network-approved site that includes anonymous visitors, **for reading only**: anonymous visitors never write anything (the 2026-09-25 decision stands), so `add: anyone` takes adds from signed-in people only |
| `signed-in` | anyone signed in on the site | anyone signed in with the company identity who may open the site: the owner, team members, named viewers at `specific`, any colleague at `company`/`listed`/`network` |
| `own` | the person who added the entry, plus the owner | the same: the entry's `added_by` (their user id) is the caller, or the caller is the owner |
| `owner` | the site owner, through their tools or signed in on the site | the site's owner or, for a team site, any team member: through their tools (connector or key) or signed in on the site itself |
| `nobody` | only the owner's tools | only the owner's tools: the connector or the owner's key, never a page |

**Tools versus pages.** A request carrying the site-host session cookie comes
from a page. A request carrying an API key or the connector's token (the MCP
server calls the site-facing routes in-process, as `get_state` does) comes
from the owner's tools. Only tools reach `nobody`, and only the owner's or a
team member's tools bypass the matrix; a colleague's key is that colleague,
under the matrix like their browser.

**The owner on their own site.** On Enterprise this is the normal case: every
view is signed in, so an owner opening their own admin page is already
recognised. That is hosted's decision D1 (owner rights for storage data only,
never settings), with no extra machinery: site-host sessions are bound to the
one host and answer only the site-facing API, so a page can never reach
settings, access, viewers, keys or deletion. Hosted had to add a frame guard;
Enterprise site hosts already refuse to be framed by any other site
(`applyOwnerHostSecurity` refuses same-site and cross-site loads that are not
top-level documents).

**Previews** stay read-only: no adds, edits or deletes from a version that is
not live.

**Who added an entry.** Visitors see `mine: true|false`, never another
person's id. Owners (tools and the owner on the site) also see `added_by`:
the username of the person who added it. Every writer is a named company
identity, and an order or a request is useless to its owner without knowing
whose it is. Who *viewed* a site remains admin-only, as decided on
2026-09-25.

## Routes

Site-facing, on the site's own host (nameless form shown; the named
`/api/sites/{site}/storage/...` works the same):

| Route | Action |
|---|---|
| `GET /api/site/storage` | the site's resources, their presets and matrices, and `you: {signed_in, site_owner}` |
| `GET /api/site/storage/kv/{name}/keys?prefix=&after=&limit=` | read (list) |
| `GET /api/site/storage/kv/{name}/keys/{key}` | read |
| `PUT /api/site/storage/kv/{name}/keys/{key}` `{"value": <json>}` | add if the key is new, edit if it exists, decided by the server under a lock |
| `DELETE /api/site/storage/kv/{name}/keys/{key}` | delete |
| `GET /api/site/storage/db/{name}/tables/{table}/rows?order=&desc=&limit=&offset=&where.{col}=` | read (list); equality filters on declared columns, at most three |
| `POST /api/site/storage/db/{name}/tables/{table}/rows` | add |
| `GET`/`PATCH`/`DELETE /api/site/storage/db/{name}/tables/{table}/rows/{id}` | read, edit, delete |
| `GET /api/site/storage/files/{name}/objects?prefix=&after=&limit=` | read (list) |
| `GET /api/site/storage/files/{name}/objects/{path...}` | read: the file itself |
| `PUT /api/site/storage/files/{name}/objects/{path...}` (raw body, `Content-Type`) | add or edit, decided by the server |
| `DELETE /api/site/storage/files/{name}/objects/{path...}` | delete |

Owner settings, on the base host (the shape of the asset routes, owner role,
`browserWrite` for the dashboard):

| Route | |
|---|---|
| `GET /api/collaboration/sites/{owner}/{sitename}/storage` | resources, matrices, bytes used and the limits |
| `PUT /api/collaboration/sites/{owner}/{sitename}/storage/resources/{name}` | create or change a resource: `kind` (`kv`, `db`, `files`), `preset` or a matrix, and for `db` its `tables` with `columns` |
| `DELETE /api/collaboration/sites/{owner}/{sitename}/storage/resources/{name}` | delete a resource and all its data |

MCP tools follow the routes: `storage_list_resources`,
`storage_set_resource`, `storage_delete_resource` (settings), and
`storage_list`, `storage_get`, `storage_put`, `storage_delete` (data through
the site-facing routes as the owner's tools, which reach `nobody`).

API key scopes: reading storage takes a publish key (`SiteAPIPattern`);
every change through a key needs a full key (`SiteAPIDeletePattern` for a
DELETE, `SiteAPIStorageWritePattern` for PUT, POST and PATCH), because a
storage entry keeps no history and a change could not be undone. Pages and
the connector are not keys. The settings routes are owner routes like the
asset routes: GET publish, PUT and DELETE full.

`GET .../storage` shows the owner every resource; anyone else sees only the
resources and tables where the matrix lets them do something, and a visible
column that references a hidden table does not name it, so the list does not
map the site's private data.

A row body is read before the write transaction opens, so a slow request
never holds the site's storage lock or a database connection.

An `own` edit or delete of an entry the caller can see but did not add
answers `404`, as for an entry they cannot see.

## Tables

```json
PUT /api/collaboration/sites/ana/bakery/storage/resources/shop
{"kind": "db", "preset": "private",
 "tables": {
   "products": {"preset": "public",
                "columns": {"name": "text", "price_cents": "integer", "photo": "text"}},
   "orders":   {"preset": "records",
                "columns": {"product_id": {"type": "integer", "references": "products"},
                            "quantity": "integer", "status": "text"}}}}
```

- Column types: `text`, `integer`, `number`, `boolean`, `json`. A column may
  be `{"type": ..., "required": true}` or `{"type": "integer", "references":
  "<table>"}` (a row id in another table of the same database).
- `id`, `added_by`, `created_at`, `updated_at` belong to the server; a request
  that sets them is refused. Unknown columns are refused, not ignored.
- A row is at most `SITE_STORAGE_VALUE_MAX_BYTES` (64 KB) of JSON as stored.
- **References respect read access.** The parent row must exist and be
  readable by the caller under the parent table's `read`; otherwise
  `404 invalid_reference`, the same as a missing parent. A row that other rows
  reference cannot be deleted (`409 referenced`); delete the children first.
  That refusal counts children the caller may not see, so it tells them one
  exists: accepted, since keeping references whole matters more than that bit.
- **Ownership is checked in the statement.** An `own` edit or delete carries
  `AND added_by = $caller` in the same `UPDATE` or `DELETE`; zero rows is
  `404`, so another person's row looks like no row.
- Changing a table's columns later is allowed: a removed column, or one given
  another type or another `references` table, loses its stored values in the
  same transaction (`cleared_columns`), so a column added back starts empty.
  A narrower rule that keeps the type (`required` added) leaves existing
  values as they are. Removing a table
  deletes its rows (the tool says so).
- A table added later uses the database's default matrix; a table that
  already exists keeps its own unless the change states one. When a change
  sets the database's preset, the answer's `notes` name each existing table
  that kept its own, so a change meant for every table is not silently
  partial.
- Numbers written with exponents are bounded before they reach Postgres,
  which stores numbers exactly (`1e100000` would be 8 bytes sent and
  100,001 digits stored): each exponent is within ±308, and the digits the
  exponents add across one body stay within the value limit.

## Limits and quota

Every limit is an env setting with hosted's value as the default:

| Setting | Default |
|---|---|
| `SITE_STORAGE_MAX_BYTES` (KV and table data, per site) | 10,000,000 |
| `SITE_STORAGE_FILES_MAX_BYTES` (files, per site) | 10,000,000 |
| `SITE_STORAGE_FILE_MAX_BYTES` (one file) | 1,000,000 |
| `SITE_STORAGE_VALUE_MAX_BYTES` (one KV value or one row) | 65,536 |
| `SITE_STORAGE_MAX_RESOURCES` (per site) | 20 |

Files also count toward `QUOTA_MAX_BYTES`. Usage is kept on the resource row
and updated in the same transaction as each change, under the site's storage
lock (a Postgres advisory lock), so the check is exact across replicas. Every
entry is measured the way Postgres stores it (`octet_length` of the jsonb
text, plus the key), on add, edit and delete alike, so the count cannot drift;
the limits are checked after the write and a refusal rolls it back. The
stored form puts a space after each `,` and `:`, so a value near the limit
can measure up to about half again what was sent. Rate limits reuse the saved-data
policies (per client and per site, shared across replicas).

## Audit

Every change is one audit row in the same transaction as the change:
`storage_resource_set`, `storage_resource_delete`, `storage_add`,
`storage_edit`, `storage_delete`, each with the resource, kind, table and the
key, path or row id (never the value). Reads are not audited; visits already
are, through the access log.

## Deliberately not in this version

- **No history for storage entries.** Saved state keeps 20 versions because
  anyone who opens a site may overwrite all of it. With presets, edits by
  other people happen only where the owner chose them (`board`, or a custom
  matrix), every change is in the audit log with who made it, and Postgres
  point-in-time recovery covers disaster. Per-entry undo can be added later
  without changing the model.
- **No raw SQL for owners** (above).
- **Erasure keeps entries.** A person erased by an admin loses their name on
  everything they added (`added_by` becomes empty): the entries stay as the
  site's data, as saved state does, and `own` actions on them fall to the
  owner.
- **No legacy translation.** Enterprise never had storage resources, so there
  are no older `read`/`write`/`write_mode` fields to translate and nothing is
  `legacy`.
- **No rebuild from the bucket.** Like saved state, storage lives in
  Postgres; `rebuild-index` restores sites, not their saved data.
- **The saved state document and assets are unchanged.** Existing pages keep
  working; new pages are pointed at storage.

## Proof

- A table-driven handler test over all 625 matrices × four callers
  (anonymous network visitor, colleague, author, owner on the site) plus the
  owner's tools × the four actions × the three kinds, checked against an
  independent reference.
- R1–R4 counts (161 valid) checked against the same reference.
- References, server columns, filters, `own` in the statement, previews,
  network visitors, publish-key scope, quota, and the audit rows.
- `make test`, `make test-db`, and a local stack run of the image with the
  owner's tools and pages.
- An independent security review before release.
