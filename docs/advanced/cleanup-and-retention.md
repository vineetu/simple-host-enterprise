# Cleanup and retention

**Recently deleted.** A deleted site is kept whole (files, saved data, access, viewers, uploaded
files) for `DELETED_RETENTION_DAYS`; its owner, a team member or an admin can restore it. Keep the
bucket's old-version retention at least as long.

**Idle-site cleanup** is off until `IDLE_CLEANUP_DAYS` is set. A site nobody has opened, deployed
to, or read or written saved data on for that long is marked: its owner sees "Not used lately"
with Keep and Download, admins see a list, and with [email](email.md) set the owner is emailed.
After `IDLE_CLEANUP_GRACE_DAYS` more, still unused and not kept, it moves to Recently deleted.
A site an admin has restricted is never marked or deleted for disuse. `IDLE_CLEANUP_DAYS` is 0
(off) or at least 30, and the grace and `DELETED_RETENTION_DAYS` at least 7, so a site unused
over a holiday is never removed.

**Logs.** The audit log, the access log and search counts are pruned by the `prune` CronJob.

The date an owner was given is stored when it is given: shortening a window never removes a site
earlier than it was told.

<!-- settings:group=cleanup -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `DELETED_RETENTION_DAYS` | `30` | 7–365 days | How long a deleted site stays restorable in Recently deleted. Keep the bucket's old-version retention at least this long. |
| `IDLE_CLEANUP_DAYS` | `0` | 0–3650 days | Idle cleanup: a site nobody opened, deployed to or saved to for this many days is marked. 0 turns it off; otherwise at least 30. |
| `IDLE_CLEANUP_GRACE_DAYS` | `30` | 7–365 days | Idle cleanup: how long a marked site waits before it moves to Recently deleted. |
| `IDLE_CLEANUP_MAX_EMAILS` | `0` | 0–100000 emails | Idle cleanup: sites one hourly run marks and emails about. 0 is no limit. |
| `AUDIT_RETENTION_DAYS` | `400` | at least 1 days | How long the audit log is kept. |
| `ACCESS_LOG_RETENTION_DAYS` | `90` | at least 1 days | How long the access log, and ended sessions with their IP and browser, are kept. |
| `SEARCH_TELEMETRY_RETENTION_DAYS` | `180` | 1–3650 days | How long search queries and clicks are kept. |
<!-- /settings -->

## Recipes

**Shorter retention.**

```
DELETED_RETENTION_DAYS=14
ACCESS_LOG_RETENTION_DAYS=30
SEARCH_TELEMETRY_RETENTION_DAYS=30
```

**Clean up abandoned sites.** `IDLE_CLEANUP_DAYS=365`, and `IDLE_CLEANUP_MAX_EMAILS=200` so the
first run over a large backlog does not send a flood.
