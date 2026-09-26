# Simple Host

A single Go service that hosts static sites for the people in one
organisation. Someone signs in through your own OIDC provider, their coding
agent publishes a directory, and the result is live at its own hostname
under that person's name (`todo.alice.<base>`), with a JSON data store and
an asset store behind it. A new site is open only to its owner (or their
team); the owner then shares it with named people or teams (404 to everyone
else), the
whole company, the company showcase, or — once an admin approves — anyone
on the network without sign-in. Every mutation and every visit is recorded
to an audit trail and access log; owners see view counts, admins see who. The same process serves the API, the sites, the
dashboard, and an MCP endpoint through which agents publish and manage
sites, teams, sharing, and site state.

This repository is the installable package: the server, its schema
migrations, a container image, and Kubernetes manifests that run on any
cluster with Postgres and an S3-compatible bucket. `docs/install.md` is the
step-by-step guide this section summarizes.

## Install with your AI agent

`INSTALL.md` is a runbook written for a coding agent. Point the agent at a
clone of this repository with access to your cluster, and it provisions the
database and bucket, writes the configuration, stops for the few steps only
a person can do (registering the OIDC app, the DNS records), applies the
manifests, and checks the result. It asks only what it cannot find out, and
never prints a secret.

```text
Read INSTALL.md in this repository and install Simple Host on our Kubernetes cluster; ask me only what you cannot find out yourself.
```

## Run it locally

To evaluate it on a laptop before a real install: Docker Desktop with
Kubernetes enabled (or minikube), `kubectl`, `kustomize`, and `mkcert`.
Then:

```sh
make local     # ingress-nginx, cert-manager, Postgres, MinIO, Dex, the app; ~5 minutes cold
make smoke     # signs in, hands off sessions between hosts, publishes and restricts a site, reads/writes state and assets
make local-down
```

The instance answers at `https://simple-host.127-0-0-1.nip.io`, which resolves
to loopback through public DNS; every owner is one label beneath it, and every
site one label beneath its owner (`todo.alice.<base>`), with a wildcard
certificate per owner that cert-manager issues on the owner's first site.
Every certificate comes from the mkcert CA, so the browser trusts them once
`mkcert -install` has been accepted. `make local` prints two Dex test
accounts (`admin@example.com` / `person@example.com`) to sign in with; see
`docs/install.md` section 4 for signing in with a real provider like Google.

## Layout

- `cmd/server` — the binary. `simple-host` serves; `simple-host version`
  prints the release, commit and schema; `simple-host migrate`
  applies the schema and is what the pod's init container runs;
  `simple-host restore` copies a stored version of any site, live or
  deleted, into a site as its next version; `simple-host migrate-storage`
  moves an older install's site volume into the bucket
  (`docs/storage.md`); `simple-host reencrypt` rewrites every stored
  object under the current envelope key, so old keys can be removed;
  `simple-host prune` drops
  expired audit/access-log partitions on a monthly `CronJob`
  (`deploy/base/cronjob-prune.yaml`), under the database's owning
  credential rather than the application's own role;
  `simple-host audit-verify` checks the audit log's hash chain;
  `simple-host owner-hosts` is the reconciler that gets each owner's
  certificate issued (`deploy/components/owner-hosts`).
- `internal/` — one package per concern. `handler` is the HTTP surface,
  `db` the queries, `storage` the bucket-backed site store and its cache, `migrate`
  the embedded schema, `oidc` sign-in, `audit` the action/access log sink
  (recorder, batching access writer, reader, and retention pruning),
  `reqlog` the request log, `mcp` the tool adapter.
- `deploy/base` — the application's manifests, including the `prune`
  CronJob. `deploy/components` add the owner-hosts reconciler, an
  in-cluster Postgres (evaluation only), MinIO, or Dex. `deploy/overlays` are environments:
  `local` is complete, `byo` / `staging` / `production` are templates for
  managed services.
- `simple-host-plugin/` — the skill bundle agents install.
- `scripts/smoke.sh` — the end-to-end check every rollout runs; `scripts/smoke-remote.sh` (`make smoke BASE=https://<base> KEY_FILE=<key file>`) is its public-HTTPS-only counterpart for a real install.
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

Sites and assets live in the bucket (`docs/storage.md`). Every object carries a server-side-encryption header (`BACKUP_SSE`, default
`AES256`) and, optionally, a client-side envelope (`BACKUP_ENVELOPE_KEY`)
encrypted before the object ever reaches the bucket.

## Development

```sh
make test      # go test ./...
make test-db   # go test ./... on a throwaway Postgres, as CI runs it
make vuln      # govulncheck
```

## More

- `INSTALL.md` — the install runbook an AI agent follows on a real cluster.
- `docs/install.md` — the full install guide, local cluster through a real
  one with your own Postgres, bucket, and OIDC provider.
- `docs/configuration.md` — every environment variable, its default, and
  the refusal it triggers when set wrong.
- `docs/security-review.md` — threat model, controls matrix, pen-test list.
- `docs/site-isolation.md` — every site on its own origin
  (`<site>.<owner>.<base>`), the fallback until an owner's certificate is
  ready, and the redirects.
- `docs/storage.md` — the bucket, the cache, encryption and key rotation.
- `docs/cloud/` — per-cloud checklists for the platform pieces a real
  install needs (AWS, GCP, Azure, Oracle Cloud, UpCloud).
- `CHANGELOG.md` — the releases.

## Licence

Apache License 2.0 — see `LICENSE` and `NOTICE`.
