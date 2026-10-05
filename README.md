# Simple Host Enterprise

Every person in a company gets a place for what their AI agent builds, at `<site>.<person>.<internal-domain>`, behind the company's sign-in.

**Try it in your organization → https://simple-host.app/setup?product=enterprise**
- **What it costs:** https://simple-host.app/costs — about 3.5–5 cents per person per month for 2,000 people on a Kubernetes cluster you already run.

- **Overview:** https://simple-host.app/enterprise
- **Brief:** https://simple-host.app/enterprise/brief
- **Architecture:** https://simple-host.app/enterprise/architecture
- **Install:** [docs/install-kubernetes.md](docs/install-kubernetes.md) (existing cluster, Helm) · [INSTALL.md](INSTALL.md) (runbook for an agent) · [docs/install.md](docs/install.md) (full guide) · [docs/](docs/)
- **Security:** [SECURITY.md](SECURITY.md) · [docs/security-review.md](docs/security-review.md)
- **Releases:** [CHANGELOG.md](CHANGELOG.md) · latest [v0.9.3](https://github.com/vineetu/simple-host-enterprise/releases/tag/v0.9.3) (numbering restarted at 0.x; v1.x releases came before it). Pin the image by digest; the current digest is in [INSTALL.md](INSTALL.md).
- **Hosted edition** (for individuals): https://simple-host.app/ · [github.com/vineetu/simple-host](https://github.com/vineetu/simple-host)

## Install

On a Kubernetes cluster you already run, with existing Postgres, an S3-compatible bucket, the company's OIDC and an ingress/issuer: [docs/install-kubernetes.md](docs/install-kubernetes.md) (Helm chart values; no cluster is created). DigitalOcean settings are the chart's `values-digitalocean.yaml`.

`INSTALL.md` is a kustomize runbook written for a coding agent. Point the agent at a clone of this repository with access to your cluster. It writes the configuration, stops for the steps only a person can do (registering the sign-in app, the DNS records), applies the manifests and checks the result. It never prints a secret.

```text
Read docs/install-kubernetes.md in this repository and install Simple Host into our existing Kubernetes cluster with Helm; ask me only what you cannot find out yourself.
```

## Features

### Identity and sign-in
- Sign-in only through the company's OIDC provider. Accounts are created at first sign-in.
- Admins come from a list of emails or an OIDC claim. There is no admin key.
- Sessions with idle and absolute limits. A person sees and revokes their sessions and connected apps, or signs out everywhere.
- API keys for CI, with a scope (`publish`, `full`, `offboard`) and an expiry; one call creates or updates a site from a pipeline ([docs/ci.md](docs/ci.md)).

### Sites, versions, preview, hand-over
- Publish a folder. Every publish is a new version; roll back to any kept version.
- Every site on its own origin: `<site>.<person>.<internal-domain>`, with a certificate issued per person.
- Preview before live: store a version without publishing, open a one-hour preview link, then make it live.
- Rename a site or move it into a team you are in (never to a person). Old addresses keep redirecting for people who can open the site.
- Delete is recoverable for 30 days. Download a whole site as one zip.
- Per-person quotas for sites and storage. Optional malware scan of every upload.
- Optional idle-site cleanup, with a warning, a Keep button and email.

### Who can see a site
- Five access levels: only me (the default), named viewers, the whole company, listed in the company showcase, and anyone on the network.
- Named viewers are people or teams, added by name or company email, including people who have not signed in yet.
- Teams: a shared place for sites; every member can do everything.
- Opening a site to the network needs an admin's approval, or two admins' if configured.
- Someone without access sees the same page as for a site that does not exist.
- Company showcase and full-text search across listed sites.

### Saved data and files
- Each site has one JSON document its pages read and write, plain or versioned.
- The last 20 changes are kept; the owner or a team member restores any of them.
- Pages upload files (images, video, PDFs and more), with per-site limits and the same malware scan.

### Agents
- An MCP server at `<base>/mcp`, connected through the company's own sign-in. Every tool call runs as that person.
- Skills for publishing and planning sites, served from the instance with version checks.
- An installable plugin at `<base>/plugin.zip`, already pointing at the instance.

### Admin and audit
- Take down: restrict any site to its owner with a reason the owner sees. Until an admin lifts it, the owner cannot raise the level, rename it or move it.
- Leavers: disable a person (sessions, keys and apps revoked at once), move their sites to a team or a person, or delete them, export their data as one zip, or erase them for good (an erased person cannot sign back in until an admin allows it). After a name change, an admin renames a person's address; old links keep working.
- Revoke a leaked key by pasting it.
- Every change is written to an audit log with a hash chain; `simple-host audit-verify` checks it.
- Every audit event is also streamed as a JSON line to stdout, for a SIEM.
- A visit log per site: owners see view counts, top pages and referring domains; admins see who.

### Operations
- Runs on any Kubernetes cluster with Postgres and an S3-compatible bucket. Manifests for local, staging and production.
- Site files live in the bucket, with server-side encryption and an optional client-side envelope. Pods are stateless and serve through a local cache.
- Backups and a tested restore drill (`simple-host restore`, `simple-host verify-storage`).
- Settings are environment variables. Nothing that identifies an install has a default, and the server refuses to start on an unsafe value.
- Health, readiness and metrics endpoints; rate limits shared across replicas.
- Per-cloud checklists in [docs/cloud/](docs/cloud/) (AWS, GCP, Azure, Oracle Cloud, UpCloud, DigitalOcean).
- A Helm chart in [deploy/helm/simple-host-enterprise](deploy/helm/simple-host-enterprise/) for an existing cluster ([docs/install-kubernetes.md](docs/install-kubernetes.md)). DigitalOcean-specific settings are an explicit preset; the Kubernetes 1-Click can pin chart 0.1.1.

## Advanced settings

Every setting, grouped by area and explained with recipes (stricter sign-in, shorter retention,
bigger uploads): [docs/advanced/](docs/advanced/README.md). The setup helper at
https://simple-host.app/setup?product=enterprise asks where it runs and should
target the Helm values in `deploy/helm/simple-host-enterprise/values.yaml` for
an existing cluster ([docs/install-kubernetes.md](docs/install-kubernetes.md)).
Anywhere else it asks a few questions, or every one in Advanced mode, and writes
`config.env`, a `secrets.env` template and the apply commands. Cloud Terraform
that can create a cluster (`deploy/terraform/aws`) is an advanced path. The helper
runs in your browser and never asks for a secret; an optional check of your choices,
just before the files, sends only the names and values of the numbers, durations,
switches and rates you changed.

## Run it locally

Docker Desktop with Kubernetes enabled (or minikube), `kubectl`, `kustomize` and `mkcert`. Then:

```sh
make local       # ingress-nginx, cert-manager, Postgres, MinIO, Dex, the app; about 5 minutes cold
make smoke       # signs in, publishes and restricts a site, reads and writes state and assets
make local-down
```

The instance answers at `https://simple-host.127-0-0-1.nip.io`. `make local` prints two test accounts (`admin@example.com` and `person@example.com`). See [docs/install.md](docs/install.md) section 4 for a real identity provider.

## Configuration

Required, with no default: the public base URL, the OIDC provider, the database, the bucket and the session signing key. Startup fails with the list of what is missing.

- [docs/configuration.md](docs/configuration.md): every variable, its default, and what it refuses.
- `deploy/overlays/byo/config.env.example` and `secrets.env.example` name every variable, the optional ones commented out at their defaults; [docs/advanced/](docs/advanced/README.md) explains each.
- [docs/storage.md](docs/storage.md): the bucket, the cache, encryption and key rotation.
- [docs/site-isolation.md](docs/site-isolation.md): every site on its own origin, and the redirects.

## Layout

- `cmd/server`: the binary and its subcommands (`migrate`, `restore`, `migrate-storage`, `reencrypt`, `verify-storage`, `rebuild-index`, `audit-verify`, `prune`, `owner-hosts`, `settings`, `version`).
- `internal/`: one package per concern (`handler`, `db`, `storage`, `migrate`, `oidc`, `audit`, `mcp`).
- `deploy/`: the manifests, optional components and environment overlays.
- `simple-host-plugin/`: the skills and plugin agents install.
- `scripts/smoke.sh`: the end-to-end check every rollout runs.
- `test/pentest`: the scripted pen-test list.

## Development

```sh
make test      # go test ./...
make test-db   # go test ./... on a throwaway Postgres, as CI runs it
make vuln      # govulncheck
```

Working rules: [AGENTS.md](AGENTS.md). What it does today, surface by surface: [FEATURES.md](FEATURES.md). Why: [INTENT.md](INTENT.md).

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

## Your home page

Your person address opens your showcase by default. In the dashboard, choose one
of your personally owned sites as home. It opens at its existing site URL, with
its ordinary OIDC hand-off and access rules; its files and saved data keep that
site's origin. An inaccessible, unpublished, deleted, taken-down home or a
pending owner certificate leaves the normal index in place. Rename follows the
site ID; deletion and transfer clear the choice. Team addresses keep their index.

Custom homes fetch `/showcase.json` on their own site host. The same feed is
available on the person host, behind company sign-in, with `Cache-Control:
no-store` and no cross-origin credential sharing. It contains `owner`, `bio` and
`sites` (`name`, `url`, `updated_at`, `pinned`, `order`), with exactly the owner
index's visibility: the owner sees all published work, colleagues see only
listed work and never restricted sites. It never changes access or listing.

Save a plain-text bio, pin sites and set manual order in the dashboard or
through the signed-in connector: `set_home_page`, `set_bio`, `set_showcase_site`.
Pinned sites come first, then ascending order, then newest update and name.
The bio limit is `SHOWCASE_BIO_MAX_LENGTH` (280 characters by default; 1–2000).
Browser and connector REST use `GET`/`PUT /api/me/home` (`{"site":"portfolio"}`
or `{"site":null}`), `GET`/`PUT /api/me/bio` (`{"bio":"My projects"}`), and
`GET`/`PUT /api/sites/{sitename}/showcase` (`{"pinned":true,"order":10}`). These
account settings require OIDC identity; CI API keys cannot change them. All
changes are audited transactionally. Settings live in Postgres; site content
stays in the bucket and pods remain stateless. No custom domains or short names.
