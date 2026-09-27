# Deploying from CI

A build pipeline publishes a site with one call and a `publish` API key.

1. Sign in to the dashboard and mint a key with the **Publish** scope (the
   default). It can deploy, update and roll back your sites and your teams'
   sites, and nothing else: it cannot delete a site, change who can open it,
   or read the audit log. Keys expire (`API_KEY_DEFAULT_DAYS`, at most
   `API_KEY_MAX_DAYS`); responses carry `X-Key-Expires` in the last
   `API_KEY_EXPIRY_WARNING_DAYS`, so a job can warn before it breaks.
2. Store it as a CI secret, for example `SIMPLE_HOST_KEY`.
3. Package the built site with `index.html` at the archive root and send it
   with `PUT ...?create=true`: the site is created the first time (`201`)
   and updated on every run after (`200`). Without `create=true` a missing
   site is `404`, so a mistyped name never creates a second site.

A personal site:

```sh
tar -C dist -czf site.tar.gz . && curl -fsS -X PUT -H "X-API-Key: $SIMPLE_HOST_KEY" -H "Content-Type: application/gzip" --data-binary @site.tar.gz "https://$SIMPLE_HOST_BASE/api/sites/my-site?create=true"
```

A team site (the key's owner must be a member of the team):

```sh
tar -C dist -czf site.tar.gz . && curl -fsS -X PUT -H "X-API-Key: $SIMPLE_HOST_KEY" -H "Content-Type: application/gzip" --data-binary @site.tar.gz "https://$SIMPLE_HOST_BASE/api/collaboration/sites/team-docs/handbook?create=true"
```

The team route requires `If-Match` with the ETag of the version being
replaced, so two people (or a person and a pipeline) cannot overwrite each
other. A pipeline that owns the site outright can read the current ETag
just before deploying:

```sh
etag=$(curl -sS -o /dev/null -D - -H "X-API-Key: $SIMPLE_HOST_KEY" "https://$SIMPLE_HOST_BASE/api/collaboration/sites/team-docs/handbook" | awk 'tolower($1)=="etag:"{print $2}' | tr -d '\r'); curl -fsS -X PUT -H "X-API-Key: $SIMPLE_HOST_KEY" -H "If-Match: $etag" -H "Content-Type: application/gzip" --data-binary @site.tar.gz "https://$SIMPLE_HOST_BASE/api/collaboration/sites/team-docs/handbook?create=true"
```

(The first deploy of a team site needs no ETag: `create=true` creates it.)

What the answers mean:

- `201` created, `200` updated; the JSON has the site's `url`. A new site
  opens only for its owner (or the team) until its access level changes.
- `409 site_exists` comes from `POST` (create) on a name that exists; use
  `PUT`, with `create=true` if the job should also create it.
- `409 name_held`: a site deleted in the last `DELETED_RETENTION_DAYS`
  holds the name. Restore it or pick another name.
- `412` on the team route: someone deployed since the ETag was read.
- `403` with a `scope` field: the call needs a Full key.
- `401` with `code` `key_expired` / `key_revoked` / `key_not_recognised`:
  mint a new key.

`?publish=false` stores the version without making it live, for a
pipeline that wants a person to preview it first (a new site's first
version is always live, so it is refused on a create). The full reference
is `/openapi.yaml` on the install.
