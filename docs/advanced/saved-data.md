# Saved data

Each site has one JSON document its pages read and write, plain (last write wins) or versioned
(compare-and-set). Every viewer who can open the site may write; API keys too. The last 20
changes are kept and the owner can restore any of them. The limits below bound reads and writes
per client and per site.

<!-- settings:group=data -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `NOTIFY_MAX_PER_SITE` | `10` | 1–1000 | Notification max per site. |
| `NOTIFY_PER_MINUTE` | `60` | 1–100000 | Notification per minute. |
| `NOTIFY_PER_DAY` | `5000` | 1–10000000 | Notification per day. |
| `NOTIFY_HISTORY_DAYS` | `7` | 1–365 | Notification history days. |
| `NOTIFY_OWNER_CONCURRENCY` | `5` | 1–200 | Notification owner concurrency. |
| `NOTIFY_PER_URL_PER_MINUTE` | `120` | 1–100000 | Notification per url per minute. |
| `NOTIFY_WORKERS` | `20` | 1–200 | Notification workers. |
| `NOTIFY_ATTEMPTS` | `5` | 1–20 | Notification attempts. |
| `NOTIFY_FIELD_MAX_BYTES` | `2048` | 1–65536 | Notification field max bytes. |
| `NOTIFY_BODY_MAX_BYTES` | `16384` | 4096–262144 | Notification body max bytes. |
| `NOTIFY_SUSPEND_ATTEMPTS` | `100` | 1–100000 | Notification suspend attempts. |
| `NOTIFY_SUSPEND_FAILURE_PERCENT` | `95` | 1–100 | Notification suspend failure percent. |
| `NOTIFY_TIMEOUT` | `5s` | 1ms–30s | Notification timeout. |
| `NOTIFY_DEFAULT_TTL` | `168h` | 1h–8760h | Notification default ttl. |
| `NOTIFY_SUSPEND_WINDOW` | `1h` | 1h–168h | Notification suspend window. |
| `NOTIFY_SUSPEND_AFTER` | `72h` | 24h–8760h | Notification suspend after. |
| `NOTIFY_ENC_KEY` | none | secret | Base64 32-byte key sealing notification signing and custom-header secrets. Empty disables notifications. **Security-sensitive.** |
| `NOTIFY_ALLOW_PRIVATE_CIDRS` | none | text | Comma-separated destination CIDRs permitted for HTTPS callbacks, including internal addresses. Empty permits public destinations only. **Security-sensitive.** |
| `RATE_LIMIT_STATE_CLIENT` | `60/1s` | any (warns past 10× looser) | Saved-data writes per client. |
| `RATE_LIMIT_STATE_SITE` | `60/1s` | any (warns past 10× looser) | Saved-data writes per site. |
| `RATE_LIMIT_STATE_READ_CLIENT` | `120/500ms` | any (warns past 10× looser) | Saved-data reads per client. |
| `RATE_LIMIT_STATE_READ_SITE` | `300/200ms` | any (warns past 10× looser) | Saved-data reads per site. |
<!-- /settings -->

## Recipes

**A busy dashboard read by many people.** Raise the per-site read limit:
`RATE_LIMIT_STATE_READ_SITE=600/100ms`.
