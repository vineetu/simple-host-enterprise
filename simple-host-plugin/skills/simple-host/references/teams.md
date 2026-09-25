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

{"name": "acme-team"}
```

The caller becomes its first member. **Only ever do this when the user asks for a
team.** Creating a team is never a side effect of a deploy, and no failure hint
should lead you into it.

Team names are typed, not derived from an email, so they take **letters, numbers
and hyphens only**. A dot is refused rather than converted: `acme.ai` is not
silently accepted as the hostname `acme-ai`. Existing dotted usernames are
untouched by this rule. A name is also refused if it is reserved, or if it is a
different spelling of a name already in use — say which name it collided with
rather than retrying variations.

A person may be a member of at most 10 teams.

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

Candidates are existing registered people who are not already members; the search
never returns teams. Add in one bounded batch, at most 50. Removal takes any
other member. Removing yourself is the same as `leave`, which answers the
remaining members, or `team_deleted: true` and `sites_deleted` when you were the
last active member (see above).

Removing someone ends their access to every site in the namespace. It does not
undo content they deployed; offer rollback separately if that is what the user
means.

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

Build with relative asset paths, and report the `url` and `public_path` the API
returns, exactly as returned.

## Wording

- "Give Bob access to the dashboard" is ambiguous. Ask whether Bob should be
  able to change it or only open it. To change it, he must be in the team that
  owns it: add him to that team (say how many sites that covers), or, for a
  personal site, offer to publish it under a team he is in. To open it, add him
  as a viewer.
- "Move my site to the team" is not supported. The address would change. Offer to
  publish a new site under the team from local source, keep the old one, and
  never delete the old site in the same turn.
- A name that is close but not exact — "deploy this to acme" when the team is
  `acme-ai` — is not a match. Show the list and ask. Never create `acme`.
