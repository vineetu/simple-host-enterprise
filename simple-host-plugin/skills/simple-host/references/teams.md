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
- A team can never be granted as a per-site editor, and never appears in editor
  candidate results.

## One role

You are in the team or you are not. There is no owner, admin, lead, or read-only
member, and nothing takes a role argument.

Any member may:

- create, update, roll back, delete, and change the listing of **any** site in
  the team's namespace;
- manage that site's per-site editor grants;
- add and remove people, including themselves;
- delete the team, once it owns no sites.

A per-site editor grant is a separate thing that still exists. An editor on a
team-owned site may deploy, download, list versions, and roll back that one site,
and nothing else. See [`collaboration.md`](collaboration.md) for the full matrix.

## A team always keeps at least one member

The last member cannot leave. `DELETE` of the last membership answers `409` with
`code: "last_member"`. The right move is to delete the team instead — or to add
someone first, then leave.

Never present leaving as a way to get rid of a team.

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
```

Candidates are existing registered people who are not already members; the search
never returns teams. Add in one bounded batch, at most 50. Removal takes any
member, including yourself, and is refused with `last_member` when it would empty
the team.

Removing someone ends their access to every site in the namespace. It does not
undo content they deployed; offer rollback separately if that is what the user
means.

### Delete

```
DELETE /api/teams/<team>
```

Any member may delete the team, and only when the team owns **zero** sites.
Otherwise it answers `409` with `code: "team_has_sites"`. Delete the sites first,
through the owner-qualified delete route, and confirm that destructive intent
with the human each time. Deleting a team is not a way to clean up its sites.

## Working on a team's sites

Nothing special. Team-owned sites are reached through the ordinary
owner-qualified collaboration routes with the team's name as `<owner>`, and every
update needs the `If-Match` ETag captured before editing. Read
[`collaboration.md`](collaboration.md).

Build against the composed long path `/sites/<team>/<sitename>/`, and report the
`url` and `public_path` the API returns, exactly as returned. Compose to build,
quote to report.

## Wording

- "Give Bob access to the dashboard", on a team-owned site, is ambiguous. Ask
  whether they mean every site under `<team>` (say how many) or only that one
  site, then choose between adding a member and granting a per-site editor. On a
  personal site it is an editor grant, as before.
- "Move my site to the team" is not supported. The address would change. Offer to
  publish a new site under the team from local source, keep the old one, and
  never delete the old site in the same turn.
- A name that is close but not exact — "deploy this to acme" when the team is
  `acme-ai` — is not a match. Show the list and ask. Never create `acme`.
