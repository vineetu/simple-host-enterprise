# CLAUDE.md

Asked to *install* Simple Host rather than develop it? Follow `INSTALL.md` instead of this file.

Guidance for coding agents working in this repository.

## What this is

The installable package of Simple Host: a Go service that hosts static sites
with a per-site JSON store, for the people of one organisation, on any
Kubernetes cluster with Postgres and an S3-compatible bucket. Read
`docs/security-review.md` and `docs/configuration.md` before changing
anything: they say what exists and what is deliberately unfinished.

## Rules

- **Read before changing:** `INTENT.md` (what this is for), then
  `FEATURES.md` (every feature and the surfaces it spans: the blast radius),
  then the relevant `docs/`.
- **Update `FEATURES.md` and `CHANGELOG.md` in the same commit** as any
  feature change. `scripts/check-features.sh` (in `make test` and CI) fails
  when a registered route or MCP tool is missing from `FEATURES.md`.
- **Parity with hosted Simple Host** (`PARITY.md`, identical in both repos):
  any security fix in one repo is checked against the other the same day
  (note it in PARITY.md). A new or changed feature updates PARITY.md in the
  same commit. `scripts/check-parity.sh` (in `make test` and CI) fails when a
  `## N.` section of `FEATURES.md` has no PARITY.md row.
- **Deploy and test before committing.** `make test`, then `make local` and
  `make smoke` for anything that touches serving, routing, the schema, or
  the manifests. Nothing is committed that has not run.
- **Short commit messages**, one or two lines. Rationale goes in code
  comments, not git history.
- **No coding-agent or model-vendor references** in commit messages.
- **Never commit credentials**, and never reintroduce anything `SCRUB.md`
  removes. Run its proof after importing code from elsewhere.
- **Edit, do not rewrite.** Simplify by editing what exists.
- **The changelog is the user's call.** Do not edit
  `internal/handler/static/changelog.html` unprompted.
- **The MCP tools follow the REST surface.** A change to a management route,
  body, header, or error contract is a change to `internal/mcp/tools.go` and
  its descriptions in the same commit. `go test ./internal/mcp/` asserts every
  tool still resolves against the router.
- **The skill follows both.** `simple-host-plugin/skills/` is what agents
  read; a stale sentence there misleads every client.

## Where things are

- `cmd/server/main.go` wires config, database, storage, handlers, the host
  gate, the request log, and the servers. `subcommands.go` is `migrate`,
  `restore`, `migrate-storage`, `reencrypt`, `prune`, `audit-verify`, and
  `owner-hosts` (the per-owner certificate reconciler in `internal/ownerhosts`).
- `internal/config` reads the environment. Required values have no default;
  the database DSN and the bucket endpoint are additionally refused if their
  TLS is weaker than required, unless the local-evaluation
  override envs are set (`DB_INSECURE_ALLOWED`, `BACKUP_STORAGE_INSECURE_ALLOWED`,
  `OIDC_INSECURE_ALLOWED`; the OIDC issuer and its discovered endpoints
  must be https otherwise).
- `internal/migrate/sql/` is the schema, applied in file order by
  `simple-host migrate`; the server refuses to start against a schema newer
  than it embeds unless every newer applied migration begins with
  `-- simple-host: backward-compatible` (additive only: tables, columns or
  indexes older code ignores), which is what makes a rollback possible. A
  release that drops a column does so one release after the code stopped
  reading it. Migration
  0020 creates `simplehost_app`, the least-privilege role the server
  connects as; `migrate` sets its password from `DB_APP_PASSWORD` every run.
- `internal/handler/host_gate.go` decides per hostname what the router may
  answer (site host, owner host, fallback path, redirects); `serve.go` serves site files; `site.go` is the management API.
- `internal/storage/store.go` is the site store: bucket objects, the live
  version the database resolves, and the pod-local cache; `objects_s3.go`
  is the S3 client (server-side-encryption header, optional client-side
  envelope); `sweep.go` deletes retired objects. See `docs/storage.md`.
- `deploy/` is kustomize: `base`, `components`, `overlays`. `make local`
  brings the `local` overlay up on Docker Desktop.

## Local development

```sh
make local && make smoke
kubectl --context docker-desktop -n simple-host logs deploy/simple-host -c simple-host
```

Every `kubectl` call names its context. The Makefile defaults to
`docker-desktop`; never let a command fall through to whatever context is
current.
