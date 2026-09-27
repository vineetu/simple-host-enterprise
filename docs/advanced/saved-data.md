# Saved data

Each site has one JSON document its pages read and write, plain (last write wins) or versioned
(compare-and-set). Every viewer who can open the site may write; API keys too. The last 20
changes are kept and the owner can restore any of them. The limits below bound reads and writes
per client and per site.

<!-- settings:group=data -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `RATE_LIMIT_STATE_CLIENT` | `60/1s` | any (warns past 10× looser) | Saved-data writes per client. |
| `RATE_LIMIT_STATE_SITE` | `60/1s` | any (warns past 10× looser) | Saved-data writes per site. |
| `RATE_LIMIT_STATE_READ_CLIENT` | `120/500ms` | any (warns past 10× looser) | Saved-data reads per client. |
| `RATE_LIMIT_STATE_READ_SITE` | `300/200ms` | any (warns past 10× looser) | Saved-data reads per site. |
<!-- /settings -->

## Recipes

**A busy dashboard read by many people.** Raise the per-site read limit:
`RATE_LIMIT_STATE_READ_SITE=600/100ms`.
