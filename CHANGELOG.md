# Changelog

Releases are published as `ghcr.io/vineetu/simple-host-enterprise:<version>`;
pin the digest, not the tag. `simple-host version` prints the running
release, commit and schema.

## Unreleased

- Rollback: the server now starts against a newer schema when every newer
  migration is marked backward-compatible, so an image can be rolled back
  without restoring the database. Rolling back to v1.0.0 itself is not
  possible once a newer migration has run.
- Each migration and its record commit in one transaction; the migrator waits
  a bounded time for its lock and runs with lock and statement timeouts.
- Prometheus metrics on port 9090 (`/metrics`), not exposed by the Service.
  `/readyz` caches its database check for 10 seconds and logs why it fails.
- Release images are signed with cosign (keyless) and no `latest` tag is
  published. The base kustomization names the published image with a
  placeholder digest that `make preflight` refuses.
- `make preflight` refuses the in-cluster evaluation Postgres outside the
  local overlay, and the server warns at startup when it runs on it.
- Per-cloud install docs split into a common template and one file per cloud.

## v1.0.0 — 2026-09-24

First release.
