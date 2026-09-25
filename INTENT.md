# INTENT

## What this is, and why it exists

Simple Host Enterprise is one Go service that gives every person in an organisation a place to
put the things their AI agent builds for them — under their own name, visible only to them until
they share it, and live the moment it exists.

AI agents already produce artifacts constantly. Every one of them lands at a random UUID on a
vendor's domain: unattributable, unfindable a week later, and hosted on someone else's internet
whether or not that is what anyone wanted. That is the problem this solves. Here the artifact
lives under its author's own subdomain, it is theirs, and it stays theirs alone until they decide
otherwise.

The result is closer to a portfolio than to a web host. The unit is the person, not the
artifact.

## Who uses it, and in what situation

- **A non-technical person who just made something.** They asked an agent for a tracker, a
  prototype, a write-up. They want it to exist at a URL, to be theirs, and to be shareable when
  they are ready. They will never read a deploy doc.
- **A technical person doing the same thing faster,** and more often.
- **Their colleagues,** looking for something a specific person made — searching that person's
  name, not a UUID they never had.
- **Whoever runs the cluster,** who needs sign-in to be the company's own, access to be
  controlled, and every publish and view to be on record.

## What success looks like

- Someone's agent publishes, and the thing is live under that person's subdomain in seconds.
  No repo, no pipeline, no YAML, no per-artifact subdomain to provision.
- A person accumulates hundreds of artifacts without that becoming a problem — one subdomain
  each, not one per artifact.
- Nothing is visible to anyone else until its author shares it. A new site is open only to its
  owner (or, for a team site, the team). The author then picks who can open it: named people or
  teams, anyone signed in at the company, the company showcase, or — with an admin's approval —
  anyone on the network. Whoever can open a site can use its saved data. The default is never a
  surprise.
- When it is shared, the author gets the credit and a colleague can find it by searching for
  the person.
- What gets published can actually *do* something — a prototype, a tracker — not just render.
- Cost stays boring on both axes: no container per artifact, and generating a static site with
  a stateful API costs a fraction of the tokens that scaffolding a real app with its own
  database does.

## Non-goals

- **Not a general web host.** No per-site custom domains. A person's subdomain is the identity;
  handing out domains would dissolve it.
- **Not a public publishing platform.** Sharing is inside the organisation, against the
  organisation's own identity provider. The one exception is a site an admin has approved for
  the network: anyone who can reach the server can read it, and nobody can change it without
  signing in.
- **Not a document tool.** Notion and Confluence write documents better. What earns a place
  here is something that runs.
- **Not a CI target.** The publisher is an agent through MCP, not a build pipeline.
- **No per-artifact infrastructure.** One object store for every site, one Postgres, one
  service. An artifact never gets its own container, database or deployment.

## Constraints

- Runs on the organisation's own cluster and infrastructure. Content does not leave it.
- Sign-in is the organisation's existing OIDC provider. No local accounts, no admin key.
- Every mutation and every visit is on record, and retention is bounded by partition pruning.
- Postgres over `sslmode=verify-full`; the object store endpoint must be HTTPS. The process
  refuses to start otherwise.
- The infrastructure floor stays small enough that the cost argument above survives contact
  with a real bill: one object store, one small Postgres, one small service, for everybody.
- Running cost and maintenance cost both stay near zero — there is nothing per-artifact to
  patch, scale or pay for. This is a supporting argument and a design constraint. It is **not**
  the selling point and must never be led with; cheap is what a product says when it has
  nothing else to say.
- Apache 2.0.

## Decisions already made

- **2026-09-23 — The unit is the person, not the artifact.** Each user owns one subdomain; all
  their work lives beneath it. Rejected: a subdomain per artifact, which does not survive
  someone making hundreds.
- **2026-09-23 — Unlisted by default** (`sites.public = false`, migration 0006). *Superseded
  by the 2026-09-25 access levels decision below; kept for history.* A new site was
  absent from search and the showcase, but any signed-in colleague with the link could open it
  and write its saved data; named viewers made it private. Most artifacts are made for their
  author. Listing is a deliberate act. (Wording corrected 2026-09-25: this line said "private",
  which the access model never was.)
- **2026-09-23 — Attribution is the discovery mechanism.** Search, AI site classification and
  `/showcase` exist so a shared artifact is findable by its author's name, and so publishing
  earns the author visibility. This is why search is in a hosting product.
- **2026-09-23 — Static pages plus a stateful API, never a per-app backend.** This is what
  makes a prototype or a tracker possible while keeping both the token cost and the
  infrastructure cost near zero.
- **2026-09-23 — Per-site state is the only data store.** The public product also has
  append-only collections; this one deliberately does not. A single append-only type is a
  primitive, not a data-store design. If artifacts ever need somewhere to write beyond their
  own state, that is a designed feature with several types, decided then — not this one
  carried over because it exists.
- **2026-09-23 — Identity is the customer's OIDC provider**, with admin following
  `ADMIN_EMAILS` / `OIDC_ADMIN_CLAIM` on a real signed-in person. No admin key exists.
- **2026-09-25 — Five access levels per site, only-me by default** (`sites.access`, migration
  0033). `only_me` (owner or team), `specific` (named people or teams, on the site's own host),
  `company` (anyone signed in with the link), `listed` (company, plus showcase and search),
  `network` (anyone who can reach the server, no sign-in). Existing sites moved to the level
  that matched what they had, so no shared link broke.
  - Only-me default: most artifacts are made for their author, so they start private to them.
  - `company` exists because a link shared inside the company is still the common case.
  - `network` is a request an admin approves or declines on /admin; the site keeps its level
    until then, and the owner can drop it at any time. Network access leaves the company's
    identity boundary, so an admin decides. Anonymous visitors can read, never write.
  - Per-site editors are removed; shared editing is publishing under a team. Editors
    duplicated teams and made one owner's origin hold other people's code.
  - The last 20 versions of each site's saved data are kept and the owner or team can restore
    one. Saved data is writable by everyone who can open the site, so it needs an undo.
  - Owners see view counts and distinct viewers, not who (`ACCESS_LOG_VISIBILITY=counts`).
    Who viewed what is sensitive, so only admins see names.
