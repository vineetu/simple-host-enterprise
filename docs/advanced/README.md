# Advanced settings

Simple Host Enterprise needs a handful of values to start: its address, your identity provider,
a Postgres database and an S3-compatible bucket. Everything else has a default, and every
default is what a release shipped with before it could be changed. This folder explains each
advanced feature in plain terms, lists every setting that changes it, and gives recipes.

**Easiest start: the setup helper at https://simple-host.app/setup** (choose Enterprise). It asks
a few questions (or every question, in Advanced mode), keeps the default for anything you skip,
and writes a `config.env` for the overlay, a `secrets.env` template naming every secret (it never
asks for a secret's value) and the commands to apply them. It runs entirely in your browser and
sends nothing anywhere.

## Where settings go

- **`deploy/overlays/<overlay>/config.env`** becomes the `simple-host-config` ConfigMap.
- **`deploy/overlays/<overlay>/secrets.env`** (never committed) becomes the `simple-host-secrets`
  Secret. Every secret can instead be mounted as a file and named with `<NAME>_FILE`
  ([../configuration.md](../configuration.md), "Secrets: environment or file").

Re-applying does not restart the pods; run
`kubectl -n simple-host rollout restart deploy/simple-host` afterwards. A value out of range or
unsafe stops the server at startup with a message naming it, and everything people and agents
are told (the dashboard, `/admin`, emails, MCP tool descriptions, the served skills) states the
value in force.

## Areas

| Area | What it covers |
|---|---|
| [Server and addresses](server-and-addresses.md) | The base address, HTTPS, the proxy in front, per-owner certificates |
| [Accounts and sign-in](accounts-and-sign-in.md) | OIDC, admins, sessions, AI app connections, API keys |
| [Access and teams](access-and-teams.md) | Access levels, network approval, teams, viewer lists, visit logs |
| [Sites and versions](sites-and-versions.md) | Upload size, versions kept, quotas, malware scan, uploaded files, preview links, search |
| [Saved data](saved-data.md) | What pages save and its limits |
| [Cleanup and retention](cleanup-and-retention.md) | Recently deleted, idle-site cleanup, audit and access log retention |
| [Email](email.md) | The SMTP relay for idle-cleanup notices |
| [Storage and backups](storage-and-backups.md) | Postgres, the bucket, encryption, the pod cache, backups |
| [Observability](observability.md) | Metrics, probes, the audit stream |
| [Rate limits](rate-limits.md) | Every rate limit in one table |

## Keeping this in sync

[settings.json](settings.json) is generated from the code (`simple-host settings --json`); a test
fails when it drifts, and `scripts/settings_docs.py --check` (in `make test` and CI) checks the
tables on these pages and [../configuration.md](../configuration.md) (the full reference, with
every refusal) against it. After changing a setting in code:

```
go run ./cmd/server settings --json > docs/advanced/settings.json && python3 scripts/settings_docs.py
```

The hosted repo keeps a copy for the setup helper; its `scripts/sync-settings.sh` refreshes it
from a checkout of this repo.
