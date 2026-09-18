# CLAUDE.md

Guidance for coding agents working in this repository.

## What this is

The installable package of Simple Host: a Go service that hosts static sites
with a per-site JSON store, for the people of one organisation, on any
Kubernetes cluster with Postgres and an S3-compatible bucket. It is built in
phases from a plan kept outside this repository; each phase's record is in
`docs/security-review.md` and `docs/configuration.md`. Read them before changing anything: they say what
exists, what deviated from the plan, and what is deliberately unfinished.

## Rules

- **Deploy and test before committing.** `make test`, then `make local` and
  `make smoke` for anything that touches serving, routing, the schema, or
  the manifests. Nothing is committed that has not run.
- **Short commit messages**, one or two lines. Rationale goes in code
  comments, not git history.
- **No coding-agent or model-vendor references** in commit messages.
- **Never commit credentials**, and never reintroduce anything `SCRUB.md`
  removes. Run its proof after importing from the source instance.
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
  `restore`, and `backup-assets`.
- `internal/config` reads the environment. Required values have no default;
  the database DSN and the bucket endpoint are additionally refused if their
  TLS is weaker than the design requires, unless the local-evaluation
  override envs are set (`DB_INSECURE_ALLOWED`, `BACKUP_STORAGE_INSECURE_ALLOWED`).
- `internal/migrate/sql/` is the schema, applied in file order by
  `simple-host migrate`; the server refuses to start unless the database
  holds exactly the versions the binary embeds. A release that drops a
  column does so one release after the code stopped reading it. Migration
  0020 creates `simplehost_app`, the least-privilege role the server
  connects as; `migrate` sets its password from `DB_APP_PASSWORD` every run.
- `internal/handler/host_gate.go` decides per hostname what the router may
  answer; `serve.go` serves site files; `site.go` is the management API.
- `internal/storage/disk.go` is the versioned site tree; `backup.go` copies
  each version to the bucket with a server-side-encryption header and an
  optional client-side envelope; `restore.go` reverses both for the
  `restore` and `backup-assets` subcommands.
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
