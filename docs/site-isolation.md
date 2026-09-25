# Site-level isolation: where we are, and the path if we ever need more

Written 2026-09-18, after the package was complete. This is a design note,
not a plan. Nothing here is scheduled.

## What we have

Isolation is per owner. Every owner has one hostname, and all of their
sites live under it as paths: `alice.<base>/todo/`, `alice.<base>/notes/`.
Owners are separate browser origins from each other, so one person's page
can never read another person's cookies, storage, or pages. A site shared
with named viewers (access level `specific`) is the exception: it is served
on its own hostname, `alice--notes.<base>`, so it is private from the
owner's other sites as well. Every other level — `only_me`, `company`,
`listed`, `network` — stays on the owner's hostname. A `network` site,
once an admin approves it, is served there without a session: anonymous
visitors can read its pages, assets and saved data, and write nothing.

Two sites under the same owner share an origin. That is deliberate. The
owner's hostname is the product: one address to remember, one thing to
type, one autofill entry, one history. It is what makes a person's pages
findable by typing their name.

## What that leaves open

Within one owner's sites there is no JavaScript boundary. A page in
`/todo/` can read `/notes/`'s DOM and storage, and can call `/notes/`'s
state API with whatever rights the visiting person has. The server checks
the person, not the page. That is acceptable because the only code on an
owner's hostname is the owner's own (for a team hostname, the team's):
nobody else can deploy there. An `only_me` site sits on that hostname for
the same reason — only the owner's or team's code runs beside it.

## The two ways to close it, and why one fits this product

**Per-site hostnames.** Give every site its own origin, the way restricted
sites already work. The plumbing exists: the host gate classifies
`owner--site` labels, the wildcard DNS and certificate cover them, nothing
is provisioned per site. It is the industry-standard answer and the one a
security reviewer accepts fastest. It was rejected for this product
because it fragments the owner's namespace: no single home, no one
address, and the identity the product is built on dissolves into a pile of
hostnames.

**Sandboxed pages with an injected token.** Keep the owner namespace and
have the browser isolate each page instead. This is the recorded path.

## How the sandbox approach would work

Two touch points, both in code that already exists.

1. **Serving.** In the host gate, after `requireHostSession` has
   authenticated the viewer and resolved the site, wrap HTML responses
   only. Add `Content-Security-Policy: sandbox allow-scripts allow-forms`
   (never `allow-same-origin`; that flag hands the real origin back and
   removes the isolation). The browser then runs the page in a unique
   opaque origin: it cannot read cookies, storage, or any other page, even
   from the same hostname.
2. **The page's own backend.** An opaque-origin page is no longer same-site
   with `alice.<base>`, so the browser will not send the session cookie on
   its state API calls. Replace the cookie with a token minted at serve
   time: sign `{session id, user id, site id, host, expiry of a few
   minutes}` with the existing session signing key and insert one meta tag
   into the HTML as it streams out. Nothing is stored and nothing is
   provisioned per site; it is derived, like the host-bound cookie is
   today. The skill's state snippet reads the meta tag and sends it as a
   bearer header.
3. **Authentication.** The auth middleware gains a third shape beside the
   session cookie and `X-API-Key`: a bearer token. Verify the signature,
   check the token's site id against the site in the path, then run the
   same session validity check (including the negative cache, so revoking
   the session kills the token) and the same viewer or writer rules.
   Handlers never learn which shape authenticated the request.
4. **Refresh.** A page open longer than a token's life presents the old
   token to the state API and gets a new one, capped by the session's own
   idle and absolute limits.

## What it costs

- **Local storage is gone.** `localStorage`, `sessionStorage` and IndexedDB
  throw in an opaque origin. This is the one thing pages actually use that
  the sandbox removes. The product already has its answer: the state API.
  The skill already steers agents toward site state instead of browser
  storage; under the sandbox that becomes a rule rather than a preference.
- **Popups, downloads and form posts** each need their own `allow-*` flag.
  Embedded third-party widgets that check their origin see `null`.
- **A bearer credential inside every HTML response.** Pages can never be
  cached by anything, ever. The `Cache-Control: private, no-cache` header
  the owner hosts already send becomes a security invariant, not a
  hygiene measure. Any proxy or CDN placed in front must honour it.
- **More code on the hottest path.** HTML rewriting while streaming, a
  third auth shape, and a refresh chain. All of it is reviewed and
  maintained forever.
- **Same-owner sites can no longer share state on purpose.** The same loss
  as per-site hostnames.

## What a security reviewer would say

They would like that the boundary is enforced by the browser, not by
application logic, and that the token is site-bound, short-lived, and
revocable with the session. They would poke at three things: a bearer
token visible in page source (answer: site-bound and short-lived, and the
same script could already act as the viewer through the cookie), the
server rewriting user HTML (a fixed string at one insertion point; show
them the code), and the refresh chain (capped by the session lifetime).
They would ask why not per-site hostnames. The answer is the namespace,
and it is a product answer, not a security one.

## If we do it

Order of work: the bearer shape in the auth middleware and its tests
first, because it is useful on its own; then the token mint and meta-tag
insertion behind a config flag, default off; then the sandbox header
behind the same flag; then the skill snippet; then run the pen-test list
against a sandboxed overlay, especially sibling-origin subresource loads
and state write across sites, which are the two items this changes.
Restricted sites keep their own hostnames regardless.
