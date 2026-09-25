---
name: simple-host-builder
description: Plan what to build on Simple Host. Walks a user through Simple Host's capabilities — static hosting, shared JSON state with version-checked saves, uploaded files, and choosing who can open a site — and produces a concrete agent prompt for whichever capability they want to add. Use when a user is starting a new Simple Host site, when they ask "what can I add to my site," or when they describe a feature idea and need help mapping it to Simple Host's primitives.
---

# Simple Host Builder

Use this skill when a user wants help deciding what to build on Simple Host, or how to add a capability to a site they already have. This skill is the **discovery surface** for Simple Host. After the user picks a capability, hand off to the `simple-host` skill (for deploy) or generate a concrete code-level prompt the user can hand to another agent.

## What Simple Host gives you

Simple Host is a server at `{{BASE_URL}}`. A site belongs to a namespace owned by a person or a team and is served at `<owner-label>.<base>/<name>/`, or at its own host once shared with named viewers. Every viewer signs in with their company account, except on a site an admin has opened to the network. Deployed pages can call the site API from the browser; there is no separate backend.

| Capability | Endpoint (from the page) | Who can use it |
|---|---|---|
| Static site hosting | the site's `url` | whoever its access level admits (only the owner or team until changed) |
| Versioned state (default for stateful sites) | `GET/PUT /api/sites/<site>/state/versioned` | anyone who can open the site (the last 20 versions can be restored) |
| Plain state (explicit last-write-wins only) | `GET/PUT /api/sites/<site>/state` | anyone who can open the site |
| Uploaded files (images, PDFs, CSV...) | `POST/GET /api/sites/<site>/assets` | anyone who can open the site |

The package ships no AI model or proxy. A prototype that needs one calls whatever endpoint the platform team provides for this installation; ask them what exists first.

## How to use this skill

1. Ask the user what they're trying to build, in plain language. Don't push capabilities at them — let them describe the idea. If the idea would put confidential material, personal data, or customer data on the site, say plainly that only content the company allows to be shared internally, or that is already public, may be hosted here — a site can be opened company-wide once it is shared — and help them scope it down or stop.
2. Map their description to one or more capabilities below. If you're unsure which fits, list two and ask them which feels closer.
3. For each capability they pick, give them: (a) a one-paragraph explanation of how it works, (b) the relevant fetch snippet, (c) the gotchas for that capability.
4. If they're starting from scratch, finish with a "ready to deploy" handoff: tell them to use the `simple-host` skill (or the Simple Host connector's tools, if present), which handles sign-in, framework-aware build, packaging, and upload. For an existing site, read its live files first — a deploy replaces every file.
5. If they want to wire a capability into a site they've already deployed, generate a focused prompt they can paste into a fresh agent chat (in their site's repo). Include the endpoint shape, auth model, and limits — nothing else.

## The capability tree

### 1. Static hosting (the baseline)

What it is: any folder of HTML/CSS/JS/assets, served as-is. No server-side execution.

When to choose: every Simple Host site starts here. Deploy first, then layer state and AI.

How to deploy: invoke the `simple-host` skill. It gets the user signed in, detects the framework, builds, packages, and uploads.

Gotchas: **build with relative asset paths (`./`)**, so the site works on its owner's host and on its own host if it is later restricted. Report only the `url` the API returns; never compose an address. For plain HTML, the `fix-paths-for-subpath-hosting` skill makes paths relative.

A page never works out its own address or site name from `location.pathname`. When it needs its site name (for the state API), write the name into the page.

### 2. Versioned per-site JSON state — the default

What it is: the same per-site JSON blob, but with a version number and compare-and-set saves. Every save bumps the version. A save says "only apply this if the version is still N" — if someone else saved in the meantime, the server rejects it with **409** and hands back the current version *and* current state, so the page re-merges and retries instead of clobbering the other person's change.

When to choose: use this automatically for **every new site that needs state**, including counters, notes, drafts, journals, settings, todos, and collaborative tools. Do not ask the user to choose between state variants. Both variants use the same JSON blob (up to 1 MiB), readable and writable by anyone who can open the site — every one of them signed in.

Agent decision rule:

- New stateful site: use `/state/versioned`.
- Existing site using plain `/state`: upgrade it when the requested work already modifies its state code; leave it unchanged during unrelated work.
- Use plain `/state` only when the user explicitly requests plain, unconditional, or last-write-wins saves.
- Call `/api/sites/<site>/state/versioned` with the site name written into the page. **Never compute the site name from `location.pathname`** and never hard-code an absolute API URL.
- On a `401`, the viewer's session expired: `location.reload()` to sign in again, don't retry.
- Never use either endpoint for secrets, PII, private drafts, or per-user data.
- Show saved data as text (`textContent`), never as HTML: another visitor wrote it.

How to use:

```js
// The site's name, written in at build time. Not read from location.pathname.
const url = '/api/sites/dashboard/state/versioned';

// load — a never-saved site reads {"version": 0, "state": {}}
const first = await fetch(url);
if (first.status === 401) location.reload(); // session expired: sign in again
let {version, state} = await first.json();

// save with merge-and-retry. `mutate` applies the user's edit to the
// freshest copy of the state, so a retry re-applies it after a conflict.
async function save(mutate) {
  for (let attempt = 0; attempt < 5; attempt++) {
    const next = mutate(structuredClone(state));
    const r = await fetch(url, {
      method: 'PUT',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({version, state: next}),
    });
    if (r.status === 401) { location.reload(); return; }
    if (r.ok) { ({version} = await r.json()); state = next; return; }
    if (r.status === 409) { ({version, state} = await r.json()); continue; } // someone saved first — re-merge
    throw new Error(`save failed: ${r.status}`);
  }
  throw new Error('too much contention — reload and try again');
}
```

Gotchas: `version` is **required** on every PUT to this endpoint (missing → 400). Write `mutate` as "apply my edit to whatever state is current," not "write my whole local copy." After a **409**, reapply that semantic mutation to the freshest returned state; never retry a stale whole snapshot. Anyone who can open the site can read or write the blob, so treat what comes back as untrusted text. Never put API keys, secrets, or PII in it.

### 2b. Plain state — explicit last-write-wins only

What it is: the same JSON blob without compare-and-set protection. Each PUT unconditionally replaces the current state, so a stale writer can silently overwrite a newer save.

When to choose: only when the user explicitly asks for plain, unconditional, or last-write-wins state. Do not choose it merely because the site appears to have one writer.

```js
const url = '/api/sites/dashboard/state'; // this site's name, written in

const state = await fetch(url).then(r => r.json());
await fetch(url, {
  method: 'PUT',
  headers: {'Content-Type': 'application/json'},
  body: JSON.stringify(state),
});
```

Gotchas: plain and versioned saves share the same blob, and plain saves still bump its version. Plain state has the same 1 MiB cap and the same audience: anyone who can open the site. Never put API keys, secrets, or PII in it.

### 3. Uploaded files

For a photo, PDF, or CSV a page wants to reference by URL, upload it with `POST /api/sites/<site>/assets` (multipart, field `file`, 25 MiB per file by default). It survives redeploys and rollbacks. See `simple-host/references/state-and-ai.md` for the contract.

### 4. Choosing who can open a site

A new site opens only for its owner (or, for a team site, the team). The owner then picks a level:

| Level | Who can open it |
|---|---|
| `only_me` | the owner, or the team's members |
| `specific` | also named people or teams; the site moves to its own address |
| `company` | anyone signed in at the company with the link |
| `listed` | company, and shown in the showcase and search |
| `network` | anyone who can reach the server, no sign-in; an admin must approve it |

Suggest `network` only when the user explicitly wants people without a company sign-in to open the site. None of these makes it a place for confidential data. To let other people change a site, publish it under a team.

## Picking a capability mix

Use this as a guide when the user describes an idea:

| User says | Capabilities to suggest |
|---|---|
| "a place I can paste my notes and they're saved" | static + versioned state |
| "a tracker/list my whole team edits at once" | static + versioned state (conflict-safe saves) |
| "I just want to host my landing page" | static only |
| "a chat UI" or "a tool that thinks" | static + versioned state, calling an AI endpoint the platform team provides; ask what exists first |
| "only my team should see it" | static, published under the team (`only_me`), or `specific` with the team as viewer |

If the user wants something Simple Host can't host (server-side execution, private per-user data, real-time multiplayer), say so explicitly. Don't try to bolt it on.

## Generating a prompt for another agent

When the user wants to wire a specific capability into an existing site, generate a focused prompt they can paste into a fresh agent chat. Keep it short — endpoint, auth model, request shape, limits. Don't dump the whole capability tree.

Example prompt for versioned state:

> Add saved notes to this site (site name `notes`). Simple Host exposes `GET/PUT /api/sites/notes/state/versioned` to the page (JSON up to 1 MiB, compare-and-set on `version`, readable and writable by anyone who can open the site). Load on start, save on change, on a `409` take the returned state, reapply my edit, and retry, and on a `401` reload the page. Show saved notes with `textContent`, never as HTML.

## Companion skill: frontend-design

Before the user starts building UI, suggest they also install Anthropic's `frontend-design` skill (a separate plugin/skill they can install in their agent). It produces distinctive, production-grade interfaces with restrained composition and avoids the generic-AI look. For Simple Host prototypes — which are typically demos for stakeholders — pairing `simple-host-builder` with `frontend-design` is the difference between "it works" and "it looks intentional."

If `frontend-design` is already loaded in the user's agent, lean on it for any visual decisions (layout, type system, motion). If it's not, mention it once and move on — don't block the build.

## Handoff: deploy

Once the user has decided what to build, they need to deploy. Tell them to use the `simple-host` skill (or include the deploy steps yourself if you know them — but the simple-host skill handles every framework and is kept up to date).

If their assets 404 after deploying, route to `fix-paths-for-subpath-hosting`. That skill detects the framework first and points back at the simple-host skill's framework-specific build instructions; it only mechanically rewrites paths for genuinely raw HTML.
