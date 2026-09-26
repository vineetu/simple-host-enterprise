# Publishing the `/enterprise` page

> **Historical.** This page and its two HTML files were written before
> v1.1 and are only partly updated: they still describe a volume for site
> files (sites now live in the bucket), a sub-path address
> (`/enterprise/`; since v1.3 a site is served at `<site>.<owner>.<base>/`),
> a repository with no public link, and a
> removed AI-gateway seam. Kept for history; do not publish them as they
> are. `README.md` and `docs/` describe the package.

This page is not part of the package and is not a route the server serves.
It is a static site, published like any other Simple Host site, under your
own account on the existing playground instance. This file is commands
only — every placeholder below is literal; nothing here has been filled in,
and nothing should be committed with real values in it.

Placeholders used throughout:

| Placeholder | What it is |
|---|---|
| `<BASE_URL>` | The playground instance's base host, e.g. `https://<your-simple-host-base-host>` |
| `<OWNER_USERNAME>` | The account this page is published under — your own username, or a team's |
| `<API_KEY>` | Your `X-API-Key` on that instance |
| `<SKILL_VERSION>` | The installed Simple Host skill's version, sent as `X-Skill-Version` |

## 1. Package

The page is two self-contained files, `index.html` and `install.html`,
each with no external scripts and no shared assets between them — every
`<style>` block is inlined in both files rather than linked, so either
page renders correctly on its own. They upload together as one site:
`index.html` is the entrypoint at `/enterprise/`, and `install.html` is
reachable at `/enterprise/install.html`, which is exactly the relative
link both pages already use to cross-link each other (`index.html`'s
"Install" nav link and "Try it" button point at `install.html`;
`install.html`'s header points back at `index.html`). Package both files
at the archive root, the same way the Simple Host skill's own packaging
step does:

```sh
tar -czf /tmp/enterprise.tar.gz -C docs/page --exclude=PUBLISH.md .
```

(`docs/page/` in this repository contains `index.html`, `install.html`,
and this file; `PUBLISH.md` is documentation, not a site asset, which is
why the command above always excludes it — do the same if you copy the
directory somewhere else before packaging.)

Confirm the archive contains exactly `index.html` and `install.html` at
its root before uploading:

```sh
tar -tzf /tmp/enterprise.tar.gz
```

## 2. Create the site

Site name is `enterprise`, under your own namespace or a team's. This is a
create, not an update — it takes no `If-Match`, because there is no prior
version to guard:

```sh
curl -i -X POST "<BASE_URL>/api/collaboration/sites/<OWNER_USERNAME>/enterprise" \
  -H "X-API-Key: <API_KEY>" \
  -H "X-Skill-Version: <SKILL_VERSION>" \
  -H "Content-Type: application/gzip" \
  --data-binary @/tmp/enterprise.tar.gz
```

A `409` means the name `enterprise` is already taken in that namespace —
relay the server's message rather than guessing a variant name. A
successful response includes `active_version` and the site's `url`; quote
that `url` exactly as returned rather than composing one yourself, since
its exact form depends on which host the request reached.

## 3. Update the page later

Every subsequent publish is an update, and the owner-qualified `PUT`
requires the current `ETag`. Capture it first:

```sh
curl -sI "<BASE_URL>/api/collaboration/sites/<OWNER_USERNAME>/enterprise" \
  -H "X-API-Key: <API_KEY>" \
  -H "X-Skill-Version: <SKILL_VERSION>" \
  | grep -i etag
```

Repackage the updated `docs/page/` (step 1), then:

```sh
curl -i -X PUT "<BASE_URL>/api/collaboration/sites/<OWNER_USERNAME>/enterprise" \
  -H "X-API-Key: <API_KEY>" \
  -H "X-Skill-Version: <SKILL_VERSION>" \
  -H "If-Match: <ETAG_CAPTURED_ABOVE>" \
  -H "Content-Type: application/gzip" \
  --data-binary @/tmp/enterprise.tar.gz
```

A `412` means something else changed the site since you captured the
`ETag` — stop, review what changed, and re-capture before retrying rather
than resending the same archive against a refreshed `ETag` blind. A `428`
means the `If-Match` header was missing or malformed; recapture the `ETag`
and retry.

## 4. Verify

Open the `url` the create or update response returned. Confirm:

- `index.html` renders, every section's anchor link (`#problem`,
  `#what-it-is`, and so on) scrolls to the right section, and the callout
  in the "Try it" section correctly says the package repository has no
  public link yet, until it does — at which point this file's "Update the
  page later" step is how you swap that callout for real links to the
  install guide and the security review pack, and this note should come
  out.
- `install.html` is reachable at the same site's `/install.html` path,
  its "Enterprise" header link returns to `index.html`, and the copy-paste
  agent prompt in its section 2 renders as plain text with no HTML
  entities showing through (if you see a literal `&lt;` or `&gt;`
  anywhere in the prompt, the archive was built from an unescaped source
  file — rebuild from `docs/page/install.html` in this repository, not
  from a hand-copied version).
- Both pages' cross-links (`index.html` → `install.html` and back) resolve
  relative to the site's own root, not to this repository — they will
  404 if the two files are ever uploaded as separate sites instead of one.

## Notes

- This page describes the *package*, not this playground instance. It
  never claims the instance you're publishing it to already runs the
  enterprise identity model — say so explicitly if you add any wording
  that could be read the other way.
- Nothing in `docs/page/` needs a build step; there is nothing to run
  before packaging beyond confirming both files are current.
- `install.html`'s copy-paste agent prompt names files and Makefile
  targets in the *package* repository, not this playground instance —
  someone who pastes it into an agent needs a checkout of the package
  repository open, not this site's own source.
