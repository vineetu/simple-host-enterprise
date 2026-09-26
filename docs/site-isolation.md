# Site-level isolation

Status: Current as of v1.3.0 (2026-09-26). Every site gets its own origin,
at `<site>.<owner>.<base>` (owner decision, 2026-09-26). Current behaviour in
full: `FEATURES.md`. Installing the certificates: `INSTALL.md` and
`docs/install.md`, "Site addresses".

## What we have

Every site, at every access level (`only_me`, `specific`, `company`,
`listed`, `network`), is served at the root of its own host:

    <site>.<owner>.<base>/        e.g. todo.alice.<base>/

Each site is its own browser origin. No page can read another site's
cookies, storage or DOM, including a sibling site of the same owner. The
site-facing API (saved data, versioned state, assets), sessions, the
`__Host-` host-only cookies, Origin checks, network-open anonymous reads and
specific-people access are all bound to the site's own host. On that host
the named API shape (`/api/sites/<site>/...`) is accepted only when it names
this site, so one site's page cannot call another site's API. The first
visit to each site host hands off from the base session transparently: one
redirect round trip, no prompt.

The owner host `<owner>.<base>/` stays the person's (or team's) index page,
behind sign-in.

### Why this form

The address reads as a sentence: the site, then whose it is, then the
company's base. Every site of one person sits under that person's name. A
dash form (`<owner>--<site>.<base>`) was considered, because one wildcard
certificate would have covered it, and rejected: it is unreadable for an
enterprise audience.

### Names

New site names are DNS labels: lowercase letters, digits and hyphens,
starting and ending with a letter or digit, at most 63 characters, and not
starting `xn--`; otherwise `400 invalid_site_name`. A new name that equals
the address of an older site of the same owner gets `409 name_conflict`.
Sites created before v1.3 keep their names; where a name is not a label
(spaces, underscores, too long), its address uses the name folded to
`[a-z0-9-]`, cut to fit, plus `-` and 6 hex digits of the name's SHA-256.
Agents never compose addresses: they quote `url` from the latest response.

### The fallback until an owner's certificate is ready

A site host needs a certificate for `*.<owner>.<base>` (below). Until that
owner's certificate is ready, the owner host serves the owner's sites itself
at `<owner>.<base>/<site>/`, the v1.2 shape, with the same sign-in, access
checks, site API and assets. Responses give that address in `url` until the
certificate is ready, then the site host. Nobody is ever sent to a host
without a certificate.

During that window the owner's sites share one origin, as they did in v1.2:
the only code on that host is the owner's (or the team's) own.

### Old addresses redirect

Every redirect is 301 for GET and HEAD and 308 for other methods, keeps the
path and query, and is computed from the address alone, with no lookup and
no sign-in, so it confirms nothing about whether a site exists.

- `<owner>.<base>/<site>/...` and `<owner>.<base>/api/sites/<site>/...`
  redirect to `<site>.<owner>.<base>/...` once the owner's certificate is
  ready.
- v1.2 site hosts `<owner>--<site>.<base>` redirect to the site's current
  address: the site host, or the fallback path before the certificate is
  ready.
- Teams are now named `team-<name>`. Old team addresses (`sales.<base>/...`,
  `sales--<site>.<base>/...`) redirect to their `team-sales` equivalents
  only while no account holds the old name; a person who later signs in as
  `sales` takes the address.

## Certificates

DNS needs nothing new: the one wildcard record `*.<base>` already matches
names at any depth, including `todo.alice.<base>`.

TLS does: a TLS wildcard covers exactly one label, so `*.<base>` covers
`alice.<base>` but not `todo.alice.<base>`. Each owner needs its own
`*.<owner>.<base>` certificate. The base certificate (`<base>` and
`*.<base>`) stays for the base and owner hosts.

With `OWNER_CERTS=auto` (the default), a separate Deployment,
`simple-host-owner-hosts` (the same image, `simple-host owner-hosts`,
component `deploy/components/owner-hosts`), runs every
`OWNER_HOSTS_INTERVAL` (15 s):

- For each owner with at least one site, it applies one Ingress
  `sh-owner-<owner>` for host `*.<owner>.<base>`, with TLS secret
  `sh-owner-<owner>-tls` and `cert-manager.io/cluster-issuer:
  <OWNER_CERT_ISSUER>`. Its ingress class, controller annotations and
  backend are copied from the install's own Ingress
  (`OWNER_INGRESS_TEMPLATE`). cert-manager issues the certificate.
- It reads that Certificate's Ready condition and records it in the
  `owner_hosts` table (migration 0042). Each server replica reads the table
  every 15 s, so an owner moves to site hosts shortly after the certificate
  is ready; for those few seconds replicas may disagree, and both answers
  work.
- When an owner has no sites left, it deletes the row first, then the
  Ingress (the Certificate goes with it; the TLS Secret is left behind and
  can be deleted by hand).

It runs apart from the server so the server's pods keep no Kubernetes
credential. Its ServiceAccount is the one token in the install; its
namespaced Role allows ingresses (get, list, create, patch, delete) and
reading cert-manager Certificates, and no Secrets. It connects to Postgres
as the application role.

With `OWNER_CERTS=manual`, the component is left out, every owner is
treated as ready, and the operator provides each owner's certificate and
ingress host rule. With `auto` and no reconciler deployed, every site keeps
serving at the fallback path.

## What it gives, and what it costs

Gives: no shared cookies, storage or DOM between an owner's sites once the
owner's certificate is ready. The browser enforces the boundary; the server
no longer relies on "only the owner's code runs here".

Costs:

- Same-owner sites can no longer share browser storage (`localStorage`,
  IndexedDB, cookies) with each other. Sites that need to share data use the
  state API.
- Each site host needs one transparent hand-off on first visit.
- One certificate per owner, and the reconciler that keeps them.

## Superseded: the sandbox plan

The 2026-09-18 note recorded a different path: keep all of an owner's sites
on one host and isolate each page with a `Content-Security-Policy: sandbox`
header plus a short-lived, site-bound bearer token injected into the HTML.
It was the alternative to per-site hostnames, rejected then because they
fragmented the owner's namespace. `<site>.<owner>.<base>` keeps every site
under the owner's name, and per-site origins close the gap with no HTML
rewriting and no bearer tokens in pages. The sandbox plan is dropped.
