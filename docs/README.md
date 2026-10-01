# Documentation

- `install.md` — installing on a local cluster, then on a real one.
- `install-kubernetes.md` — Helm chart into an existing Kubernetes cluster
  (existing Postgres, bucket, OIDC, issuer; no cluster is created).
- `cloud/` — per-cloud notes for managed Kubernetes, Postgres and buckets.
- `advanced/` — every setting by area, in product terms, with recipes; the
  tables are generated from `advanced/settings.json` (`simple-host settings
  --json`). The setup helper at https://simple-host.app/setup (choose
  Enterprise) builds `config.env` and a `secrets.env` template from it.
- `configuration.md` — every environment variable the server reads, with every
  refusal.
- `ci.md` — deploying a site from a build pipeline with one call and a
  publish-scoped API key.
- `uninstall.md` — removing a real install: what to export first, DNS
  records before the load balancer, the cluster, the identity provider,
  the bucket, the database and the secrets kept elsewhere.
- `storage.md` — the bucket, the pod cache, encryption and key rotation.
- `security-review.md` — the controls, the pen-test list and what was run,
  and the accepted limitations.
- `site-isolation.md` — every site on its own origin (`<site>.<owner>.<base>`),
  the fallback until an owner's certificate is ready, and the redirects.
- `page/` — historical: an early marketing page, kept for history, not current.

The top-level `README.md` says how to run the package locally, `CLAUDE.md`
and `AGENTS.md` carry the working rules, and `SCRUB.md` is the checklist that
keeps identifiers of the code this package was derived from out of this tree.
