# Teams

Read this file completely before creating a team, changing who is in one,
deleting one, or acting on a site a team owns.

A team is a **namespace**, exactly like a person. It owns sites, its name is part
of the address those sites are served at, and it appears as `owner_username` on
the collaboration routes.

A team is **not** a person:

- **A team has no API key, ever.** There is nothing to log in as. You always act
  with your own personal key, and the server resolves your access to the team's
  namespace on each request.
- Never try to sign a team in or mint it an API key. A team has no sign-in of
  its own; every member acts on the team's sites with their own personal key.
  See [`account-recovery.md`](account-recovery.md).
- A team can be named as a viewer of a site (level `specific`), which lets its
  members open that site but not change it.

## One role

You are in the team or you are not. There is no owner, admin, lead, or read-only
member, and nothing takes a role argument.

Any member may:

- create, update, roll back, and delete **any** site in the team's namespace;
- set who can open it and manage its viewers, and restore its saved data;
- add and remove people, and leave;
- delete the team, and with it every site it owns.

Being in the team is the only way to change a team's sites. A new team site
opens only for the team's members until its level changes; see
[`collaboration.md`](collaboration.md).

## When the last active member leaves, the team closes

A team lasts while somebody who can sign in is in it. Members whose accounts an
admin has disabled do not count. When the last active member leaves, the team
**and every site it owns** are deleted.

Because that destroys sites, leaving (or removing yourself) as the last active
member is refused until the team's name is typed back: the first call answers
`409` with `code: "confirm_team_delete"` and `site_count`. Tell the person how
many sites would be deleted, in those words, and ask. Only if they agree, repeat
the call with `?confirm_name=<team>`. Never add `confirm_name` on your own, and
never present leaving as a tidy way to get rid of a team's sites. If they would
rather keep the sites, someone else has to be added first.

A team whose members are all disabled is deleted by a company admin from the
admin page.

## Lifecycle

All routes are authenticated with the acting person's key and the current
`X-Skill-Version`.

### Create

```
POST /api/teams
Content-Type: application/json

{"name": "acme"}
```

The caller becomes its first member. Every team's name begins with `team-`:
sending `acme` or `team-acme` both create `team-acme`, and the response `name`
is the one to use from then on (in `<owner>`, in `simple-host.json`, and when
talking to the user). The team routes below accept either spelling; the
collaboration routes take the name as the API returns it, so take names from
`GET /api/teams` or the site list rather than typing them. **Only ever do this when the user asks for a
team.** Creating a team is never a side effect of a deploy, and no failure hint
should lead you into it.

Team names are typed, not derived from an email, so they take **letters, numbers
and hyphens only**. A dot is refused rather than converted: `acme.ai` is not
silently accepted as the hostname `acme-ai`. Existing dotted usernames are
untouched by this rule. A name is also refused if it is reserved, or if it is a
different spelling of a name already in use — say which name it collided with
rather than retrying variations.

A person who already belongs to 10 teams cannot create another (`409`
`team_limit`). A team has at most 50 members (`409` `member_limit`).

### List

```
GET /api/teams
```

Lists the teams the caller belongs to. `GET /api/me` also carries them, as
`teams: [{name, id}]`, alongside the caller's own namespace `id`; that is what
the `owner_id` in a project's `simple-host.json` is checked against.

### Members

```
GET /api/teams/<team>/members
GET /api/teams/<team>/member-candidates?q=<text>
POST /api/teams/<team>/members      {"usernames": ["person.one", "person.two"]}
DELETE /api/teams/<team>/members/<username>
POST /api/teams/<team>/leave
```

Candidates are registered people, each marked `already_member`; the search
never returns teams. Add in one bounded batch, at most 50. A company email
works wherever a username does: it adds the person whose account carries it,
or, if they have not signed in yet, a pending member (listed with
`"pending": true` and the email as `username`, counted toward the 50,
removed by that email) who joins at their first sign-in. An email outside the
company's sign-in domains is refused (`400` `invalid_email`). Removal takes any
other member. Removing yourself is the same as `leave`, which answers the
remaining members, or `team_deleted: true` and `sites_deleted` when you were the
last active member (see above).

Removing someone ends their access to every site in the namespace. It does not
undo content they deployed; offer rollback separately if that is what the user
means.

### Hand a site over

```
POST /api/collaboration/sites/<owner>/<site>/transfer   {"to": "team-sales"}
```

Moves a site to a team the caller is in, or to any person who can still sign
in (`to` is their exact username). Its versions, saved data and history,
uploads, access level (an admin's restriction included) and viewers all go
with it; only the owner, and so the
address, changes. The response carries the new `url` and `previous_url`; the old
address redirects to the new one until a site takes the old name again. `404`
`destination_not_found` means no such person or team you are in; `409`
`name_conflict` means the receiver already has a site by that name (rename one
with `POST .../rename {"name": "..."}` first); `409` `name_held` means a
recently deleted site of the receiver still holds the name (restore it or pick
another name); a recently deleted site itself is `404` until restored; `409`
`site_limit` or `413`
`storage_quota` means the receiver has no room. Connector tools: `transfer_site`,
`rename_site`. A full-scope key or a connector is needed; a publish key cannot.

When leaving would delete a team, the `409 confirm_team_delete` says so and
offers this: move the sites worth keeping first (to another team the person is
in, or to the person), then leave. Ask which sites to keep.

### Delete

```
DELETE /api/teams/<team>
```

Any member may delete the team. Its sites are deleted with it, so a team that
owns any answers `409` with `code: "confirm_team_delete"` and `site_count` until
the call carries `?confirm_name=<team>`. Say how many sites go with it and get
the person's agreement first. The name is released afterwards; this cannot be
undone.

## Working on a team's sites

Nothing special. Team-owned sites are reached through the ordinary
owner-qualified collaboration routes with the team's name as `<owner>`, and every
update needs the `If-Match` ETag captured before editing. Read
[`collaboration.md`](collaboration.md).

Each team site is served on its own host, like any other site. Build with
relative asset paths, and report the `url` and `public_path` the API returns,
exactly as returned.

## Wording

- "Give Bob access to the dashboard" is ambiguous. Ask whether Bob should be
  able to change it or only open it. To change it, he must be in the team that
  owns it: add him to that team (say how many sites that covers), or, for a
  personal site, offer to publish it under a team he is in. To open it, add him
  as a viewer.
- "Move my site to the team" is a transfer (see "Hand a site over" below). Confirm
  the team, move it, and report the new `url`; the old address redirects.
- A name that is close but not exact — "deploy this to acme" when the team is
  `team-acme-ai` — is not a match. Show the list and ask. Never create `team-acme`.
