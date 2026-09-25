# Scrub checklist

This repository began as a copy of an internal instance's working tree. The
copy carried that instance's hostnames, account identifiers, namespace and
secret names, and operational detail, none of which belongs in a package that
other organisations install. This file is the checklist that was run before
the first commit, kept here so it can be run again after any import from that
instance.

## What to remove

| Class | Examples of what was found | Where it went |
|---|---|---|
| Hostnames | the public base host and its parents, the staging host | `simple-host.example.com`, `simple-host.127-0-0-1.nip.io` |
| Account and resource ids | the cloud account number, IAM role ARNs, certificate ARNs, KMS key ids, load-balancer log buckets | removed with the manifests that carried them |
| Namespaces | the Kubernetes namespace of the original instance | `simple-host` |
| Secret names | the secret-store path the config loader read | removed with the loader; Kubernetes Secrets only |
| Profile and context names | cloud CLI profiles, kubeconfig contexts | removed; the Makefile names `docker-desktop` |
| Gateway URLs | the internal AI gateway and its portal | removed with the proxies |
| Bucket names | the original backup bucket | `simple-host-backups` |
| Support channel | the internal chat channel pages pointed at | removed; pages and skills say "your platform team" |
| Email domain | the corporate domain in placeholders, tests and one migration backfill | `@example.com`; the backfill was dropped |
| Brand names | product and design-system names in comments and pages | plain words |
| People | real usernames in docs, tests and scripts | `alice`, `bob`, `acme-team` |
| Credentials | keys, passwords, tokens, DSNs | none may exist; the scanner below proves it |
| Docs | every folder under `docs/` describing the original instance | dropped; this package's docs start at `docs/README.md` |

## The proof

Run from the repository root. Every command must print nothing (the grep) or
report clean (the scanner). Terms are the ones that identified the original
instance; add to the list, never remove from it.

```sh
grep -rIi --exclude-dir=.git --exclude=SCRUB.md \
  -e playstation -e kcloud -e nurture-ai -e osiris -e "usm/svc" -e skuba \
  -e "sony.com" -e dev16 -e elblog -e 789576034278 -e "simple-host-staging" .

gitleaks detect --source . --no-git --redact

go test ./...
```

`internal/config/config.go` must contain no default that names an
environment: every value that identifies an installation is required and the
process refuses to start without it.

## What was deliberately kept

- `internal/handler/static/changelog.html` is the product's release history.
  Two lines were reworded to satisfy the grep; the entries themselves stand.
- Example team and person names in tests and skill text are fictional.
- The design lineage the package's plan cites lives in the source
  repository's `docs/`, not here.
- The Go module path `github.com/vsriram/simple-host` is an import path, not
  an identifier of the original instance. It changes in one mechanical
  rename when the repository is given a public home.
