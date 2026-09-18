# Simple Host

A single Go service that hosts static sites for the people in one
organisation. Someone signs in through your own OIDC provider, their coding
agent publishes a directory, and the result is live at that person's own
hostname with a JSON data store and an asset store behind it. A site can be
restricted to named viewers, in which case it moves to its own dedicated
hostname and refuses everyone else with a 404. Every mutation and every
visit is recorded to an audit trail and access log an owner or admin can
read back or export. The same process serves the API, the sites, the
dashboard, and an MCP endpoint so agents reach every operation through one
tool surface.

This repository is the installable package: the server, its schema
migrations, a container image, and Kubernetes manifests that run on any
cluster with Postgres and an S3-compatible bucket. `docs/install.md` is the
step-by-step guide this section summarizes.

## Run it locally

Docker Desktop with Kubernetes enabled (or minikube), `kubectl`, `kustomize`,
and `mkcert`. Then:

```sh
make local     # ingress-nginx, cert-manager, Postgres, MinIO, Dex, the app; ~5 minutes cold
make smoke     # signs in, hands off sessions between hosts, publishes and restricts a site, reads/writes state and assets
make local-down
```

The instance answers at `https://simple-host.127-0-0-1.nip.io`, which resolves
to loopback through public DNS; every owner is one label beneath it, and a
restricted site gets its own `<owner>--<site>` label under the same wildcard
certificate. The certificate comes from mkcert, so the browser trusts it once
`mkcert -install` has been accepted. `make local` prints two Dex test
accounts (`admin@example.com` / `person@example.com`) to sign in with; see
`docs/install.md` section 4 for signing in with a real provider like Google.

## Layout

- `cmd/server` — the binary. `simple-host` serves; `simple-host migrate`
  applies the schema and is what the pod's init container runs;
  `simple-host restore` rebuilds a site version (and its assets) from the
  bucket; `simple-host backup-assets` is what its CronJob runs to sync
  every site's assets directory to the bucket; `simple-host prune` drops
  expired audit/access-log partitions on a monthly `CronJob`
  (`deploy/base/cronjob-prune.yaml`), under the database's owning
  credential rather than the application's own role.
- `internal/` — one package per concern. `handler` is the HTTP surface,
  `db` the queries, `storage` the site tree, backups and restores, `migrate`
  the embedded schema, `oidc` sign-in, `audit` the action/access log sink
  (recorder, batching access writer, reader, and retention pruning),
  `reqlog` the request log, `mcp` the tool adapter.
- `deploy/base` — the application's manifests, including the
  `backup-assets` and `prune` CronJobs. `deploy/components` add an
  in-cluster Postgres, MinIO, or Dex. `deploy/overlays` are environments:
  `local` is complete, `byo` / `staging` / `production` are templates for
  managed services.
- `simple-host-plugin/` — the skill bundle agents install.
- `scripts/smoke.sh` — the end-to-end check every rollout runs.
- `test/pentest` — the scripted half of the pen-test list in
  `docs/security-review.md`, gated behind the `pentest` build tag and a
  live local overlay.

## Configuration

Everything that identifies an installation is required and has no default:
the public base URL, the OIDC provider, the database, the bucket, the
session signing key. Startup fails with the list of what is missing.
`deploy/overlays/byo/config.env.example` and `secrets.env.example` name
every variable; `docs/configuration.md` documents each one in full.

The database DSN must use `sslmode=verify-full` with a root certificate, and
the bucket endpoint must be `https://`; the process refuses to start
otherwise, unless `DB_INSECURE_ALLOWED` / `BACKUP_STORAGE_INSECURE_ALLOWED` opt
into the weaker mode a local evaluation cluster uses. The server itself
connects to Postgres as `simplehost_app`, a least-privilege role migration
0020 creates; migrations always run as the owning role instead. There is no
admin key and no synthetic admin principal: admin status follows
`ADMIN_EMAILS`/`OIDC_ADMIN_CLAIM` on a real signed-in person.

Backups carry a server-side-encryption header (`BACKUP_SSE`, default
`AES256`) and, optionally, a client-side envelope (`BACKUP_ENVELOPE_KEY`)
encrypted before the object ever reaches the bucket.

## Development

```sh
make test      # go test ./...
make test-db   # the migration chain on a throwaway Postgres
make vuln      # govulncheck
```

## More

- `docs/install.md` — the full install guide, local cluster through a real
  one with your own Postgres, bucket, and OIDC provider.
- `docs/configuration.md` — every environment variable, its default, and
  the refusal it triggers when set wrong.
- `docs/security-review.md` — threat model, controls matrix, pen-test list.
- `docs/site-isolation.md` — why sites under one owner share an origin, and the recorded path to per-page isolation if it is ever needed
- `docs/cloud/` — per-cloud checklists for the platform pieces a real
  install needs (AWS, GKE, AKS, Oracle Cloud).

## Licence

Apache License 2.0 — see `LICENSE` and `NOTICE`.
