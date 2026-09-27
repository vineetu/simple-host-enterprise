# Access and teams

Every site has one of five access levels: **only me** (the default), **specific** people and
teams, the whole **company**, **listed** in the company showcase, or **network** (anyone who can
reach it, no sign-in). Opening a site to the network needs an admin's approval, or two different
admins' with `NETWORK_ACCESS_APPROVALS=2`. Someone without access sees the same page as for a
site that does not exist.

**Teams** are shared places for sites: `team-<name>`, one role, every member can do everything.
Members and viewers can be added by company email before they have ever signed in.

**Visit logs.** A site's owner sees view counts by default; `ACCESS_LOG_VISIBILITY` can show them
who, or nothing (admins always see who).

<!-- settings:group=access -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `NETWORK_ACCESS_APPROVALS` | `1` | `1` / `2` | How many different admins must approve opening a site to the network (no sign-in). **Security-sensitive.** |
| `MAX_TEAMS_PER_PERSON` | `10` | 1–1000 teams | Teams one person may belong to before they can create another. |
| `MAX_TEAM_MEMBERS` | `50` | 1–1000 members | Members of one team, pending ones included. |
| `MAX_SITE_VIEWERS` | `50` | 1–1000 entries | People and teams on one site's viewer list. |
| `ACCESS_LOG_VISIBILITY` | `counts` | `counts` / `owner` / `admin` | What a site's owner sees of its visits: counts (views and distinct viewers), owner (each visit and who), or admin (nothing; admins only). **Security-sensitive.** |
| `RATE_LIMIT_ADMIN_CLIENT` | `10/10s` | stricter freely; loosest `40/2.5s` | Admin actions per client address. **Security-sensitive.** |
| `RATE_LIMIT_ADMIN_IDENTITY` | `10/10s` | stricter freely; loosest `40/2.5s` | Admin actions per admin. **Security-sensitive.** |
<!-- /settings -->

## Recipes

**Two-person rule for network access.** `NETWORK_ACCESS_APPROVALS=2`.

**Owners never see who visited.** `ACCESS_LOG_VISIBILITY=admin`.
