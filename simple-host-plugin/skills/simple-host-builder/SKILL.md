---
name: simple-host-builder
description: Plan what to build on Simple Host. Walks a user through Simple Host's capabilities — static hosting, JSON state persistence with version-checked saves by default, AI forwarders (Claude chat with extended thinking and image inputs, OpenAI chat, voice transcription) — and produces a concrete agent prompt for whichever capability they want to add. Use when a user is starting a new Simple Host site, when they ask "what can I add to my site," or when they describe a feature idea and need help mapping it to Simple Host's primitives.
---

# Simple Host Builder

Use this skill when a user wants help deciding what to build on Simple Host, or how to add a capability to a site they already have. This skill is the **discovery surface** for Simple Host. After the user picks a capability, hand off to the `simple-host` skill (for deploy) or generate a concrete code-level prompt the user can hand to another agent.

## What Simple Host gives you

Simple Host is a single Go server at `https://simple-host.example.com`. A site belongs to a namespace owned by a person or a team, and is served at **two** addresses: `<base>/sites/<owner>/<name>/...` and `<owner-label>.<base>/<name>/...`, where the owner label is the owner's name with dots turned into hyphens. One site, two addresses. The server also exposes two state endpoints that any deployed site can call from the browser. There is no separate backend infrastructure.

| Capability | Endpoint | Auth from the browser |
|---|---|---|
| Static site hosting | `/sites/<owner>/<name>/...` and `<owner-label>.<base>/<name>/...` | none |
| Versioned state (default for stateful sites) | `GET/PUT /api/site/state/versioned` (relative path, no site name) | none; the server identifies the calling site from the page's own address (forgeable Referer attribution only) |
| Plain state (explicit last-write-wins only) | `GET/PUT /api/site/state` (relative path, no site name) | none; the server identifies the calling site from the page's own address (forgeable Referer attribution only) |

Static + versioned state cover prototypes that need to remember things. A prototype that needs a model calls whatever AI endpoint the platform team provides for this installation; the package ships none.

## How to use this skill

1. Ask the user what they're trying to build, in plain language. Don't push capabilities at them — let them describe the idea. If the idea would put confidential material, personal data, or customer data on the site, say plainly that only content the company allows to be shared internally, or that is already public, may be hosted here — any signed-in person in the company can view an unrestricted site — and help them scope it down or stop.
2. Map their description to one or more capabilities below. If you're unsure which fits, list two and ask them which feels closer.
3. For each capability they pick, give them: (a) a one-paragraph explanation of how it works, (b) the relevant fetch snippet, (c) the gotchas for that capability.
4. If they're starting from scratch, finish with a "ready to deploy" handoff: tell them to use the `simple-host` skill, which handles registration, framework-aware build, packaging, and upload.
5. If they want to wire a capability into a site they've already deployed, generate a focused prompt they can paste into a fresh agent chat (in their site's repo). Include the endpoint shape, auth model, and limits — nothing else.

## The capability tree

### 1. Static hosting (the baseline)

What it is: any folder of HTML/CSS/JS/assets, served as-is. No server-side execution.

When to choose: every Simple Host site starts here. Deploy first, then layer state and AI.

How to deploy: invoke the `simple-host` skill. It gets the user signed in and holding an API key, detects the framework, sets the right base path, builds, packages, and uploads.

Gotchas: **compose to build, quote to report.** Always build against the composed long path `/sites/<owner>/<name>/`, so absolute paths like `/assets/app.js` do not break — a build made for the long path works at *both* addresses and never needs rebuilding, while a build made for the short one 404s on the base host. But never *report* an address you composed: the `url` and `public_path` the API returns are the ones to hand a user, and their spelling depends on the host the request arrived on. The `simple-host` skill handles the build side for every common framework. For plain HTML, the `fix-paths-for-subpath-hosting` skill is the fallback.

A page must never work out its own address, either. Do not read the site or owner name out of `location.pathname` and do not hard-code an absolute API URL — both break the moment the page is loaded on the short address. Call the platform's endpoints by their relative paths and let the server identify the caller.

### 2. Versioned per-site JSON state — the default

What it is: the same per-site JSON blob, but with a version number and compare-and-set saves. Every save bumps the version. A save says "only apply this if the version is still N" — if someone else saved in the meantime, the server rejects it with **409** and hands back the current version *and* current state, so the page re-merges and retries instead of clobbering the other person's change.

When to choose: use this automatically for **every new site that needs state**, including counters, notes, drafts, journals, settings, todos, assistants, and collaborative tools. Do not ask the user to choose between state variants. Both variants use the same public, non-confidential JSON blob (up to 1 MiB); authenticated state is deferred until Okta.

Agent decision rule:

- New stateful site: use `/state/versioned`.
- Existing site using plain `/state`: upgrade it when the requested work already modifies its state code; leave it unchanged during unrelated work.
- Use plain `/state` only when the user explicitly requests plain, unconditional, or last-write-wins saves.
- Call the state endpoints by their nameless relative paths, `/api/site/state/versioned` and `/api/site/state`. The server identifies the calling site from the page's own address (the `Referer` the browser sends), on both the long and the per-owner short address. **Never compute the site name from `location.pathname`** and never hard-code an absolute API URL. The older `/api/sites/<user>/<name>/state[/versioned]` routes keep working for existing sites but must not be used for new code.
- Never use either endpoint for secrets, PII, private drafts, or per-user data. Browser-generated keys do not make public state private.

How to use:

```js
// Relative, nameless path: the server works out which site is calling from
// the page's own address. Do not derive the site name from location.pathname.
const url = '/api/site/state/versioned';

// load — a never-saved site reads {"version": 0, "state": {}}
let {version, state} = await fetch(url).then(r => r.json());

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
    if (r.ok) { ({version} = await r.json()); state = next; return; }
    if (r.status === 409) { ({version, state} = await r.json()); continue; } // someone saved first — re-merge
    throw new Error(`save failed: ${r.status}`);
  }
  throw new Error('too much contention — reload and try again');
}
```

Gotchas: `version` is **required** on every PUT to this endpoint (missing → 400). Write `mutate` as "apply my edit to whatever state is current," not "write my whole local copy." After a **409**, reapply that semantic mutation to the freshest returned state; never retry a stale whole snapshot. The server identifies the calling site from the page's own address (the `Referer` the browser sends) for attribution and accidental-misuse friction, but the header is forgeable and is not authentication or authorization. A caller can read or write the blob without loading the site. Never put API keys, secrets, or PII in it.

### 2b. Plain state — explicit last-write-wins only

What it is: the same public JSON blob without compare-and-set protection. Each PUT unconditionally replaces the current state, so a stale writer can silently overwrite a newer save.

When to choose: only when the user explicitly asks for plain, unconditional, or last-write-wins state. Do not choose it merely because the site appears to have one writer.

```js
// Relative, nameless path: the server works out which site is calling from
// the page's own address. Do not derive the site name from location.pathname.
const url = '/api/site/state';

const state = await fetch(url).then(r => r.json());
await fetch(url, {
  method: 'PUT',
  headers: {'Content-Type': 'application/json'},
  body: JSON.stringify(state),
});
```

Gotchas: plain and versioned saves share the same blob, and plain saves still bump its version. Plain state has the same 1 MiB cap and the same public, unauthenticated, forgeable Referer attribution. Never put API keys, secrets, or PII in it.

## Picking a capability mix

Use this as a guide when the user describes an idea:

| User says | Capabilities to suggest |
|---|---|
| "a place I can paste my notes and they're saved" | static + versioned state |
| "a tracker/list my whole team edits at once" | static + versioned state (conflict-safe saves) |
| "I just want to host my landing page" | static only |
| "a chat UI" or "a tool that thinks" | static + versioned state, calling an AI endpoint the platform team provides; ask what exists first |

If the user wants something Simple Host can't host (server-side execution, persistent per-user accounts, file storage beyond 1 MiB JSON, real-time multiplayer), say so explicitly. Don't try to bolt it on.

## Generating a prompt for another agent

When the user wants to wire a specific capability into an existing site, generate a focused prompt they can paste into a fresh agent chat. Keep it short — endpoint, auth model, request shape, limits. Don't dump the whole capability tree.

Example prompt for versioned state:

> Add saved notes to this site. The Simple Host server hosting it exposes `GET/PUT /api/site/state/versioned` (relative path, JSON up to 1 MiB, compare-and-set on `version`). Load on start, save on change, and on a `409` take the returned state, reapply my edit, and retry.

## Companion skill: frontend-design

Before the user starts building UI, suggest they also install Anthropic's `frontend-design` skill (a separate plugin/skill they can install in their agent). It produces distinctive, production-grade interfaces with restrained composition and avoids the generic-AI look. For Simple Host prototypes — which are typically demos for stakeholders — pairing `simple-host-builder` with `frontend-design` is the difference between "it works" and "it looks intentional."

If `frontend-design` is already loaded in the user's agent, lean on it for any visual decisions (layout, type system, motion). If it's not, mention it once and move on — don't block the build.

## Handoff: deploy

Once the user has decided what to build, they need to deploy. Tell them to use the `simple-host` skill (or include the deploy steps yourself if you know them — but the simple-host skill handles every framework and is kept up to date).

If their build is broken on subpath, route to `fix-paths-for-subpath-hosting`. That skill detects the framework first and points back at the simple-host skill's framework-specific build instructions; it only mechanically rewrites paths for genuinely raw HTML.
