# Hosted-page state, assets, and search capabilities

Read this file completely before telling a user that a Simple Host page needs a
separate backend. These are browser-callable platform endpoints, distinct from
authenticated site-management APIs, and every one of them now requires a
signed-in viewer (or an `X-API-Key`) — there is no anonymous or Referer-based
path any more.

For request/response examples and product-design guidance, also invoke the
`simple-host-builder` skill.

## Capability map

| Capability | Endpoint | Contract |
|---|---|---|
| Public site discovery | `GET /api/search?q=<query>` | Requires a signed-in base-host session. Returns at most one best active HTML match per `public=true`, unrestricted site. |
| Versioned shared state | `GET/PUT /api/sites/{site}/state/versioned` | Default for stateful pages. Shared JSON up to 1 MiB with compare-and-set saves. |
| Plain shared state | `GET/PUT /api/sites/{site}/state` | Last-write-wins only. Use only when the user explicitly requests plain/unconditional state. |
| Uploaded assets | `POST/GET /api/sites/{site}/assets`, `DELETE /api/sites/{site}/assets/{id}`, `GET /{site}/_assets/{id}[/{name}]` | Files a page can reference (images, PDFs, audio/video, plain text/CSV/JSON, zip/gzip) that live outside the site's own version history. |

## Identity, not Referer

The state and asset routes are reached on the page's own address (an owner's
`<label>.<base>/<site>/...`, or a restricted site's own
`<owner>--<site>.<base>/...`) and authenticate the caller from their signed-in
session cookie, exactly like viewing the page itself. Referer is never read
anywhere on this surface any more. `{site}` is a real path segment: compute it
from the page's own address with `location.pathname.split('/')[1]` on an
owner host — the split is meaningless on a restricted site's own host, which
has no `/<site>/` segment at all, so a page that might be restricted should
have its own site name baked in at deploy time rather than derived from the
URL. Never hard-code an absolute API URL; always call a relative path so the
same code works on whichever host the page is currently served from.

A `401` from either the state or the asset routes means the viewer's session
expired or the page is now being viewed from a different host than expected.
The correct recovery is to reload the page (`location.reload()`), which
re-enters the host's sign-in hand-off; retrying the same fetch with the same
credentials will not succeed.

```js
const site = location.pathname.split('/')[1];

async function loadState() {
  const response = await fetch(`/api/sites/${site}/state/versioned`);
  if (response.status === 401) { location.reload(); return; }
  const { version, state } = await response.json();
  return { version, state };
}

async function saveState(version, state) {
  const response = await fetch(`/api/sites/${site}/state/versioned`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ version, state }),
  });
  if (response.status === 401) { location.reload(); return; }
  if (response.status === 409) {
    const conflict = await response.json();
    // Reapply the user's semantic mutation to conflict.state, then retry
    // saveState(conflict.version, mergedState). Never retry the old
    // snapshot unchanged.
    return conflict;
  }
  return response.json();
}
```

`PUT` (and any other non-`GET` call) needs no extra header when the browser's
own session cookie is doing the authenticating — the server checks `Origin`
against the page's own host automatically. An agent calling these routes
directly with `curl` and `X-API-Key` instead of a browser session is exempt
from that `Origin` check, which is how an agent seeds or reads a site's state
without a browser at all.

## Uploaded assets

Use the asset routes for a file a page wants to reference by URL — a photo, a
PDF, an audio or video clip, a CSV or JSON data file — rather than folding it
into the site's own version history. An asset survives a redeploy or a
rollback that a file bundled into the site's `.tar.gz`/`.zip` would not.

```js
async function uploadAsset(file) {
  const form = new FormData();
  form.append('file', file);
  const response = await fetch(`/api/sites/${site}/assets`, { method: 'POST', body: form });
  if (response.status === 401) { location.reload(); return; }
  const { id, url } = await response.json();
  return url; // <img src="${url}"> or similar
}

async function listAssets() {
  const response = await fetch(`/api/sites/${site}/assets`);
  if (response.status === 401) { location.reload(); return; }
  const { assets } = await response.json();
  return assets; // [{ id, name, content_type, size, url, created_at }, ...]
}
```

The upload is refused (`415`) if the file's actual bytes do not sniff as
`image/*`, `video/*`, `audio/*`, `application/pdf`, JSON, CSV, plain text, a
zip, or a gzip archive — a relabeled HTML file is still refused, because the
allowlist checks the sniffed bytes, never the declared type or the file
extension. A site archive may not contain a top-level `_assets/` entry;
naming an upload that way is refused at deploy time, before it ever reaches
the asset store. Default limits are 25 MiB per file and 500 MiB / 5,000 files
per site; an installation may raise or lower both.

An asset is served back with the stored content type and, for anything other
than an image, video, audio clip, or PDF, `Content-Disposition: attachment` —
so a page that wants to *display* a downloaded file (an image, a PDF preview)
gets exactly that, and everything else downloads instead of executing.

## State defaults

Choose versioned state automatically for every new stateful site. Do not ask
the user to choose between variants. If requested work modifies an existing
site's plain-state implementation, upgrade it to versioned state; preserve
plain state during unrelated work.

On a versioned-state `409`, take the freshest returned state, reapply the
user's semantic mutation, and retry against that version. Never retry an old
whole snapshot unchanged.

Never store API keys, passwords, tokens, confidential business data, personal
data, or other secrets in hosted state or in an uploaded asset: state and
assets are readable by anyone the site's viewer rule admits, which is "any
signed-in person" unless the site has been explicitly restricted to named
viewers.

## Public search

Search requires the caller to be signed in on the base host; there is no
anonymous search session any more. Only sites set to `public=true` **and**
not restricted to named viewers are discoverable — a restricted site never
appears in search results, regardless of its `public` flag. A direct URL
loading does not mean a site is opted into search.

## AI

The package ships no model and no AI proxy. A page that needs one calls an
endpoint the platform team has provided for this installation; ask them what
exists before designing around it. Never embed a management `X-API-Key`, a
cloud credential, or a provider secret in HTML or JavaScript, and treat model
output as untrusted content before inserting it into the DOM.

## Cross-origin isolation

Every owner is served on their own address (`<label>.<base>`), and a
restricted site moves to its own dedicated address
(`<owner>--<site>.<base>`) the moment it gains its first named viewer. A page
cannot read or write another owner's state or assets: the browser's session
cookie is host-only, and the server checks `Origin` on every write. Sites
belonging to the *same* owner still share that owner's origin on purpose (so
they can deliberately share state), which is unchanged.
