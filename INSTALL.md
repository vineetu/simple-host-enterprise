# Install Simple Host (runbook for an AI agent)

This file is written for a coding agent that has been pointed at this
repository and asked to install Simple Host on a company's Kubernetes
cluster. It is a runbook: follow it top to bottom. `docs/install.md` is the
human guide it summarises, and the source of detail when a step here is
short.

Rules for the whole run:

- **Ask the human only what Step 0 lists.** Discover everything else.
- **Never print a secret.** Not to the terminal, not in a summary, not in a
  diff. Secrets go straight from a generator or a hidden prompt into a
  gitignored file. To check a secret file, use the status command in
  section 4, which prints key names and lengths only.
- **Never commit** `config.env`, `secrets.env`, `db-ca.crt`, or any edited
  overlay file. The first three are gitignored; confirm with
  `git check-ignore` before you write to them.
- **Name the kubectl context on every call** (`kubectl --context "$CTX" ...`,
  `make ... INSTALL_CONTEXT="$CTX"`). Never let a command fall through to
  whatever context happens to be current.
- **Stop at every HUMAN STEP.** Hand over the checklist with the exact values
  filled in, then wait.

Shell variables do not survive between separate tool calls in most agents.
Every command below either re-derives what it needs or asks you to
substitute a value you already know.

## Goal, and what "done" means

A working Simple Host at `https://<base>` on the company's cluster, using
its own Postgres, its own S3-compatible bucket, and its own OIDC provider.
Done means all of these hold:

1. `curl -fsS https://<base>/readyz` returns `{"status":"ok"}` over a
   publicly trusted certificate (no `-k`, no custom CA).
2. A person in `ADMIN_EMAILS` signs in at `https://<base>/auth/login`, and
   `https://<base>/api/me`, opened in the same browser, shows
   `"is_admin": true`.
3. `make smoke BASE=https://<base> KEY_FILE=...` (section 9) passes
   against the real install, and the browser check after it works.
4. The `simple-host-prune` CronJob can pull its image (section 9, last
   check).

## Step 0. Ask the human

Ask these in one message. Do not ask anything else up front.

1. **Where does it run?** An existing Kubernetes cluster on AWS (EKS),
   Google Cloud (GKE), Azure (AKS), Oracle Cloud (OKE), or other. Which
   kubectl context, if there is more than one.
2. **Base domain.** The hostname the dashboard lives at, e.g.
   `corp-sites.com`. Every person gets `<name>.<base>` and every site
   `<site>.<name>.<base>`, so the company
   must control DNS for `<base>` and `*.<base>`. It must be a separate
   registrable domain from the company's other apps (`corp-sites.com`, not
   `simple-host.corp.com`): hosted pages are written by anyone in the
   company, and under the company's main domain they would be same-site
   with its other apps, able to receive and plant cookies scoped to that
   domain.
3. **Identity provider.** Okta, Microsoft Entra ID, Google Workspace, or
   another OIDC provider.
4. **Admin emails.** The addresses that get admin rights.
5. **Allowed email domains.** Who may sign in at all, e.g. `example.com`.
6. **Database and bucket.** May you create a managed Postgres and a bucket in
   the company's cloud account, or are there existing ones to use?

Everything else (cluster version, ingress controller, cert-manager, storage
classes, cloud CLI identity, region) you find out yourself in section 1.

## 1. Preflight

Run these from the repository root. Each has a fix if it fails.

| Check | Command | If it fails |
|---|---|---|
| Tools | `for t in kubectl kustomize openssl curl git python3; do command -v $t >/dev/null \|\| echo "missing: $t"; done` | Install the missing tool. `kustomize` must be the standalone binary: the `Makefile` calls `kustomize build`. |
| Contexts | `kubectl config get-contexts` | Ask the human which context, if Step 0 did not settle it. Set `CTX` to it. |
| Kubernetes version | `kubectl --context "$CTX" version` | Server 1.30 or later: the Deployment's `preStop` uses the native `sleep` action (on by default from 1.30). Older clusters refuse the pod; upgrade the cluster. |
| Cluster reachable | `kubectl --context "$CTX" cluster-info` | Fix credentials first (`aws eks update-kubeconfig`, `gcloud container clusters get-credentials`, `az aks get-credentials`, `oci ce cluster create-kubeconfig`). A timeout rather than an auth error: some clouds (UpCloud, for one) start a managed cluster's API with an empty IP allow-list; add the address you run `kubectl` from. |
| Permission to install | `kubectl --context "$CTX" auth can-i create namespace` and `kubectl --context "$CTX" auth can-i create clusterrole` | Namespace is required. ClusterRole is only needed if ingress-nginx or cert-manager must be installed; otherwise ask the platform team to install them. |
| Cloud identity | `aws sts get-caller-identity`, `gcloud config list account`, `az account show`, or `oci iam region list` | Needed only if you are creating the database or bucket. Ask the human to sign the CLI in. |
| Ingress controller | `kubectl --context "$CTX" get ingressclass` | None: install the cloud's managed ingress (section 1, Cluster & ingress, of `docs/cloud/<cloud>.md`). ingress-nginx is retired upstream (best-effort maintenance ended March 2026, no further security fixes); `make install` still adds it when there is no IngressClass at all. Any existing class (ALB, GKE, Traefik, AGIC, …): `make install` uses it and installs nothing. Section 5 step 3 covers a cluster with several classes. |
| cert-manager | `kubectl --context "$CTX" get crd certificates.cert-manager.io` | Missing: `make prereqs INSTALL_CONTEXT="$CTX"` installs it. |
| DNS-01 issuer | `kubectl --context "$CTX" get clusterissuer` | None that can solve DNS-01 for `<base>`: a wildcard certificate cannot use HTTP-01. See HUMAN STEP C. |
| Storage classes | `kubectl --context "$CTX" get storageclass` | Note the default, and whether an encrypting class exists. See section 5 step 5. |
| Namespace free | `kubectl --context "$CTX" get namespace simple-host` | Exists already: find out whether it is an earlier attempt before you touch it. |
| Local files safe | `git check-ignore deploy/overlays/byo/config.env deploy/overlays/byo/secrets.env deploy/overlays/byo/db-ca.crt` | Must print all three paths. If not, stop: `.gitignore` has been changed. |
| OIDC issuer | `curl -fsS "<issuer>/.well-known/openid-configuration" \| python3 -m json.tool \| head` | Wrong issuer URL. See HUMAN STEP A for each provider's issuer. |
| OIDC issuer from the cluster | `kubectl --context "$CTX" -n default run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration` (after install, repeat with `-n simple-host` if egress is restricted per namespace) | The pods have no egress to the issuer. The server discovers it at startup and exits if it cannot, so the pod would crash-loop. Fix the egress path (firewall, proxy, private DNS) first. |

The overlay is `deploy/overlays/byo`. `staging` and `production` are the
same shape with no files of their own yet (`docs/install.md` section 5,
last paragraph); use `byo` unless the human asks for one of them.

## 2. Provision the database

Managed Postgres 16, with TLS enforced and point-in-time recovery on
(7 days' retention or more recommended). Nothing in this package backs up
Postgres, so PITR is required, not optional. The in-cluster Postgres
component is evaluation-only: `make preflight` refuses it outside
`deploy/overlays/local`. The per-cloud specifics are in section 3
(Postgres) of `docs/cloud/aws.md`, `docs/cloud/gcp.md`,
`docs/cloud/azure.md`, and `docs/cloud/oci.md`.

What the application needs:

- A database named `simplehost` (or whatever you set as `DB_NAME`).
- An owning role (`DB_USER`, e.g. `simplehost`) that can create tables and
  **create roles**: migration `0020` creates `simplehost_app`, the
  least-privilege role the server connects as. A cloud's admin user has
  this. Use the `DB_PASSWORD` you generate in section 4 as this role's
  password, so the value never has to be typed or shown.
- Reachable from the cluster's pods on its port. Use the port the provider
  gives you as `DB_PORT` (5432 on most clouds; UpCloud uses 11569).
- TLS with a CA bundle you can download. Save it as
  `deploy/overlays/byo/db-ca.crt`. The server refuses anything but
  `sslmode=verify-full` with a root certificate.
- **Storage encryption at rest on**, which also covers its snapshots and
  point-in-time-recovery backups. The database holds people's email
  addresses, every site's saved data and history, and the access and audit
  logs. Some clouds cannot turn it on after creation (AWS RDS), so set it
  when you create the database; section 3 of each cloud file says how.

Azure note: some subscriptions refuse PostgreSQL Flexible Server outright.
Read section 3 (Postgres) of `docs/cloud/azure.md` before you try.

Check reachability from inside the cluster (prints nothing secret):

```sh
kubectl --context "$CTX" run pgcheck --rm -i --restart=Never --image=postgres:16.11 -- pg_isready -h <db-host> -p <db-port>
```

## 3. Provision the bucket and pick the image

**Bucket.** Any S3-compatible bucket over `https://`, with versioning and a
lifecycle rule turned on (`docs/storage.md`): it holds every site. Details per
cloud in section 4 (Bucket) of each `docs/cloud/*.md`:

- AWS: S3. Leave the key pair empty and use IRSA or EKS Pod Identity on the
  `simple-host` ServiceAccount (see section 5 step 6).
- GCP: Cloud Storage through its S3-compatible XML API. Needs an HMAC key
  pair; there is no workload-identity path for that API.
- Azure: Blob Storage has no S3 API and this package has no native Blob
  backend yet, so Azure needs an S3-compatible object store. Read section 4
  (Bucket) of `docs/cloud/azure.md` and settle it with the human before going
  further.
- Oracle: Object Storage's S3 Compatibility API with a Customer Secret Key.
  The endpoint contains the tenancy's object-storage namespace.

**Image.** Use the current release, `ghcr.io/vineetu/simple-host-enterprise:v1.3.0`
(public, linux/amd64 and linux/arm64), pinned by its digest:

```
sha256:313d18cea526a1f2ea0b01ade46682d14bbe33972f3611730c11567e6358a7d7
```

That is what section 5 step 1 puts in the overlay. The release workflow
(`.github/workflows/release.yml`) publishes every `v*` tag, and
`sha-<full commit>` tags, the same way; there is no `latest` tag.
`CHANGELOG.md` lists the releases. Pin by digest, never by tag. To use
a different build, list the published tags:

```sh
curl -s -H "Authorization: Bearer $(curl -s 'https://ghcr.io/token?scope=repository:vineetu/simple-host-enterprise:pull' | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')" https://ghcr.io/v2/vineetu/simple-host-enterprise/tags/list
```

Prefer the newest `v*` tag. Otherwise use the tag for the commit you have
checked out (`sha-$(git rev-parse HEAD)`) if it exists, or build and push your own from this commit (`make image`, then tag
and push to a registry the cluster can pull from). The image's migrations
must be at least as new as any database it has touched, unless every newer
migration is marked backward-compatible (`docs/install.md` section 10).

Resolve the digest (either command):

```sh
docker buildx imagetools inspect ghcr.io/vineetu/simple-host-enterprise:<tag>
```

```sh
curl -sI -H "Authorization: Bearer $(curl -s 'https://ghcr.io/token?scope=repository:vineetu/simple-host-enterprise:pull' | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')" -H 'Accept: application/vnd.oci.image.index.v1+json' https://ghcr.io/v2/vineetu/simple-host-enterprise/manifests/<tag> | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}'
```

Verify the signature before you pin a release digest. Release images from
v1.1.0 on are signed keyless with cosign from the release workflow; this must
print a verified result, not an error. No `cosign` on the machine: download
the release binary for its OS and architecture from
https://github.com/sigstore/cosign/releases (a single file; `chmod +x` it)
or install it with the OS package manager.

```sh
cosign verify ghcr.io/vineetu/simple-host-enterprise@sha256:<digest> --certificate-identity-regexp '^https://github.com/vineetu/simple-host-enterprise/\.github/workflows/release\.yml@refs/tags/v' --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

v1.0.0 predates signing; check its build-provenance attestation instead:
`gh attestation verify oci://ghcr.io/vineetu/simple-host-enterprise@sha256:<digest> --repo vineetu/simple-host-enterprise`.

Each release also carries an SBOM and a provenance attestation. Read the SBOM
with:

```sh
docker buildx imagetools inspect ghcr.io/vineetu/simple-host-enterprise@sha256:<digest> --format '{{ json .SBOM }}'
```

If the company mirrors images into a private registry, the pull secret goes
on the ServiceAccount, not the Deployment (`docs/install.md` section 10,
"If you use a private registry"), and on the owner-hosts reconciler's
ServiceAccount too (section 6, "Site addresses").

## 4. Write config.env and secrets.env

```sh
d=deploy/overlays/byo; for f in config.env secrets.env; do [ -e "$d/$f" ] || cp "$d/$f.example" "$d/$f"; done; chmod 600 "$d/secrets.env"
```

**config.env** holds no secrets; edit it directly. Every variable is
documented in `docs/configuration.md`. Values are unquoted `KEY=value`
lines (kustomize reads them as env files).

| Variable | Value |
|---|---|
| `PUBLIC_BASE_URL` | `https://<base>`, origin only, no trailing path |
| `OWNER_CERT_ISSUER` | The ClusterIssuer that signs each owner's `*.<owner>.<base>` certificate (section 6, "Site addresses") |
| `SECURE_MODE`, `PORT`, `HTTPS_REDIRECT_PORT` | Leave as in the example |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID` | From HUMAN STEP A |
| `OIDC_SCOPES` | Leave at `openid email profile` unless the provider notes say otherwise |
| `ADMIN_EMAILS` | Step 0 answer, comma-separated |
| `ALLOWED_EMAIL_DOMAINS` | Step 0 answer, comma-separated. Required with Google (the server refuses to start without it: empty would let any Google account sign in). With another provider, empty means whoever the provider issues a token for, and the server logs a reminder at startup |
| `TRUSTED_PROXY_CIDRS` | The range the ingress controller's pods get their addresses from, so rate limits and logs see each person's address instead of the ingress's. Read the pod IPs with `kubectl --context "$CTX" -n <ingress-namespace> get pod -o wide` and use the pod network range they fall in (for example `192.168.0.0/16`). A node's `.spec.podCIDR` is not always that range: CNIs such as Cilium in cluster-pool mode assign pod IPs from their own pool |
| `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER` | From section 2 |
| `DB_SSLMODE`, `DB_SSL_ROOT_CERT` | Leave as `verify-full` and `/etc/simple-host/db-ca/ca.crt` |
| `BACKUP_STORAGE_ENDPOINT`, `BACKUP_STORAGE_REGION`, `BACKUP_STORAGE_BUCKET`, `BACKUP_STORAGE_PREFIX` | From section 3 and section 4 (Bucket) of `docs/cloud/<cloud>.md` |
| `BACKUP_SSE` | `AES256`, or `aws:kms` plus `BACKUP_SSE_KEY_ID` on AWS with a customer key |

Never set `DB_INSECURE_ALLOWED`, `BACKUP_STORAGE_INSECURE_ALLOWED` or
`OIDC_INSECURE_ALLOWED` on a real install.

### Internal-only installs

Nothing in the package checks where the install is reachable from. If it
sits on an internal-only ingress, the cloud AI apps (ChatGPT, Claude on the
web) cannot reach its `/mcp` at all, since their servers call it from the
internet. Say so in config too: set `OAUTH_REDIRECT_HOSTS` to only the apps
you use inside the network, for example `localhost` (command-line agents on
the person's own machine) plus `vscode.dev` or `cursor://anysphere.cursor-mcp`
if those are in use. The default list includes `chatgpt.com` and
`claude.ai`; an app whose redirect host is not listed can register but can
never finish connecting. The certificate in "done" (top of this file) still has to be
publicly trusted (a DNS-01 issuer works for a name only reachable inside),
and section 8's check runs from any machine on the network that is not in
the cluster.

### Sessions and leavers

Simple Host asks the IdP about a person only when they sign in. Set
`SESSION_TTL` and `SESSION_IDLE` (defaults `8h` and `30m`) to match the
IdP's own session policy: they are how long someone disabled at the IdP can
keep working here. `OAUTH_REFRESH_TTL` (default 30 days) does the same for an
AI app connected to `/mcp`. All of them are in `docs/configuration.md`.

Offboarding is two steps: disable the person in the IdP, **and** disable
them in Simple Host, which ends their sessions, API keys and connected apps
at once. Either press Disable on the `/admin` page, or let HR automation do
it with an offboard key (an admin mints one on `/dashboard`, scope
"Offboard"; it can call this one route and nothing else):

```sh
curl -fsS -X POST "$SIMPLE_HOST_URL/api/admin/users/disable" -H "X-API-Key: $SIMPLE_HOST_OFFBOARD_KEY" -H 'Content-Type: application/json' -d "{\"email\": \"$LEAVER_EMAIL\"}"
```

It answers 200 (`disabled`, or `already disabled` on a repeat), 404 for an
address no account has, and is recorded in the audit log against the key.

Pods do not restart when `config.env` or `secrets.env` change. After any
later edit, re-apply and run
`kubectl --context "$CTX" -n simple-host rollout restart deploy/simple-host`.

**secrets.env.** Generate the three random values in place. This prints
nothing and only fills keys that are still empty, so it is safe to re-run:

```sh
f=deploy/overlays/byo/secrets.env; SK="k1:$(openssl rand -base64 32)" EK="k1:$(openssl rand -base64 32)" DP="$(openssl rand -hex 24)" AP="$(openssl rand -hex 24)" awk 'BEGIN{FS=OFS="="} $1=="SESSION_SIGNING_KEY"&&$2==""{$2=ENVIRON["SK"]} $1=="BACKUP_ENVELOPE_KEY"&&$2==""{$2=ENVIRON["EK"]} $1=="DB_PASSWORD"&&$2==""{$2=ENVIRON["DP"]} $1=="DB_APP_PASSWORD"&&$2==""{$2=ENVIRON["AP"]} {print}' "$f" > "$f.new" && mv "$f.new" "$f" && chmod 600 "$f"
```

If the database already exists with an owner password someone else set,
leave `DB_PASSWORD` for the human to enter with the hidden prompt below, and
remove it from the command above first.

When you create the database yourself, do not put the owner password on
the command line: a command-line argument is visible to every user of the
machine (`ps`) and can land in the CLI's debug logs. Prefer the provider's
managed-password option and copy the generated value into `DB_PASSWORD`
with the hidden prompt below (AWS RDS: `--manage-master-user-password`,
then read it from Secrets Manager). Otherwise create the database with a
throwaway password and set the real one from a `psql` session with
`\password <owner role>`, which prompts for it and sends only a SCRAM
hash to the server.

Values that come from a person (the OIDC client secret; a bucket key pair
on GCP, Oracle, or wherever there is no workload identity) are entered by
the human with this hidden prompt, run from the repository root. Change
`k=` to the variable name:

```sh
(k=OIDC_CLIENT_SECRET; f=deploy/overlays/byo/secrets.env; printf '%s: ' "$k"; read -rs v && K="$k" V="$v" awk 'BEGIN{FS=OFS="="} $1==ENVIRON["K"]{$0=ENVIRON["K"] "=" ENVIRON["V"]} {print}' "$f" > "$f.new" && mv "$f.new" "$f" && chmod 600 "$f" && echo " saved")
```

Check which secrets are set, without showing any value:

```sh
awk -F= '/^[A-Z_]+=/{n=length($0)-length($1)-1; print $1 ": " (n>0 ? "set (" n " chars)" : "empty")}' deploy/overlays/byo/secrets.env
```

Required before apply: `OIDC_CLIENT_SECRET`, `SESSION_SIGNING_KEY`,
`DB_PASSWORD`, `DB_APP_PASSWORD`. The two `BACKUP_STORAGE_*` keys are set
together or both left empty.

`BACKUP_ENVELOPE_KEY` (client-side encryption of every site object, so the
bucket alone cannot read them) is generated by the command above. **Escrow
it in the organisation's secret store before the first deploy.** Every site
is readable only with it: lose it and every site is lost, and bucket
versioning does not help, since every version is encrypted under the same
key. To run without it, empty it before the first deploy; do not add or
remove it later without reading `docs/storage.md` (Encryption).

## 5. Edit the overlay

All in `deploy/overlays/byo/`. These files are tracked; do not commit your
edits to them.

1. `kustomization.yaml`, `images:` entry (name `ghcr.io/vineetu/simple-host-enterprise`): set `newName` to
   `ghcr.io/vineetu/simple-host-enterprise` (or your mirror) and `digest`
   to the verified digest from section 3 (for v1.3.0, `sha256:313d18cea526a1f2ea0b01ade46682d14bbe33972f3611730c11567e6358a7d7`).
   The base names the image with the placeholder digest
   `sha256:REPLACE_WITH_THE_RELEASE_DIGEST`, which `make preflight` refuses.
2. `ingress-patch.yaml`: replace every `simple-host.example.com` with
   `<base>` (both the TLS hosts and the two rules, including the
   `*.` wildcard). Set `cert-manager.io/cluster-issuer` to the DNS-01
   ClusterIssuer's name.
3. Same file: `ingressClassName` is left unset, which uses the cluster's
   default IngressClass (or the ingress-nginx `make install` adds when there
   is none). If the cluster has several classes and none is the default, or
   you want a non-default one, uncomment it and set a name from
   `kubectl get ingressclass`. Keep only the request-body-size and timeout
   annotations for that controller (the file lists nginx, Traefik, AWS ALB,
   GKE, and Azure AGIC). Uploads are up to 100 MiB; a controller left at its
   default refuses them and it looks like an application bug.
4. `db-ca.crt`: the database CA bundle from section 2.
5. Storage: sites live in the bucket; pods keep only a disposable cache.
   Turn on the bucket's versioning and lifecycle rule before the first
   deploy (`docs/storage.md`, bucket requirements).
6. Workload identity for the bucket (AWS IRSA / Pod Identity, or any cloud
   whose S3 endpoint accepts it): annotate the `simple-host` ServiceAccount
   with a patch in `kustomization.yaml`. The base ServiceAccount is
   `deploy/base/serviceaccount.yaml`. On EKS with IRSA, for example:

   ```yaml
   patches:
     - path: ingress-patch.yaml
     - target: {kind: ServiceAccount, name: simple-host}
       patch: |
         - op: add
           path: /metadata/annotations
           value: {eks.amazonaws.com/role-arn: "arn:aws:iam::<account>:role/<role>"}
   ```

Then render and run the repository's own checks:

```sh
kustomize build deploy/overlays/byo >/dev/null && echo renders
```

```sh
make preflight OVERLAY=deploy/overlays/byo INSTALL_CONTEXT="$CTX"
```

It fails on any placeholder left in `kustomization.yaml`, `config.env`, or
`ingress-patch.yaml` (`REPLACE_WITH…`, `example.com`, `example-idp`,
naming the lines), on an ingress class the cluster does not have, and on
the in-cluster Postgres component in any overlay but `local`.

## 6. HUMAN STEPS

Stop here and give the human this checklist with every `<...>` filled in.
Do not continue until they confirm A and B (and C, if it applies).

### A. Register the OIDC application

Generic settings for any provider:

- Application type: web application, Authorization Code flow. The server
  uses PKCE (S256) and sends the client secret in the token request body
  (`client_secret_post`).
- Redirect URI, exactly: `https://<base>/auth/callback`
- Sign-in / initiate-login URI (if asked): `https://<base>/auth/login`
- Scopes: `openid email profile`. The ID token must carry an `email` claim
  (or set `OIDC_EMAIL_CLAIM` to the claim that holds it) and be signed
  RS256.
- Give the agent the **issuer URL** and **client ID** (not secret). Enter
  the **client secret** with the hidden prompt in section 4.

Provider notes:

- **Okta.** Applications → Create App Integration → OIDC - OpenID Connect →
  Web Application. Put the redirect URI under "Sign-in redirect URIs".
  Assign the people or groups who may sign in. Issuer:
  `https://<your-okta-domain>` for the org authorization server, or
  `https://<your-okta-domain>/oauth2/default` for the default custom one;
  whichever you pick, its `/.well-known/openid-configuration` must load.
  Client authentication: "Client secret", token endpoint auth method
  `client_secret_post` (the server sends the secret in the request body).
  Do not pick `private_key_jwt`; the server does not support it.
  Optional admin by group: add a groups claim to the ID token, then set
  `OIDC_ADMIN_CLAIM=groups` and `OIDC_ADMIN_VALUE=<group name>`.
- **Microsoft Entra ID.** App registrations → New registration, single
  tenant ("Accounts in this organizational directory only"); a multi-tenant
  app would admit accounts from other tenants. Redirect URI: platform "Web". Certificates & secrets → New client
  secret; copy the **Value**, not the Secret ID, and note its expiry.
  Issuer: `https://login.microsoftonline.com/<tenant-id>/v2.0` (the
  `/common` and `/organizations` endpoints are refused at startup). Entra only
  puts `email` in the ID token when the user has a mail address: add the
  `email` optional claim under Token configuration, or set
  `OIDC_EMAIL_CLAIM=preferred_username` if sign-in names are email
  addresses. Also add the `xms_edov` optional claim there: Entra sends no
  `email_verified`, and without `xms_edov` every sign-in is refused as
  unverified. Optional admin by app role: set `OIDC_ADMIN_CLAIM=roles` and
  `OIDC_ADMIN_VALUE=<role value>`.
- **Google Workspace.** Follow `docs/install.md` section 4 with the real
  base URL. In short: OAuth consent screen **Internal**; Credentials →
  OAuth client ID → Web application; authorised JavaScript origin
  `https://<base>`; redirect URI as above. Issuer:
  `https://accounts.google.com`. Google has no groups in the ID token, so
  `ADMIN_EMAILS` is the only admin control.
- **Any other OIDC provider.** Same generic settings. Every OIDC call the
  server makes (discovery, token exchange, JWKS) goes from the pod to the
  provider, so the issuer must be reachable from inside the cluster. If it
  is not, the pod exits at start and crash-loops (section 1, the in-cluster
  issuer check).

### B. DNS records

Two records, both pointing at the ingress controller's external address:

| Name | Type | Value |
|---|---|---|
| `<base>` | `A` (or `CNAME` / alias) | `<ingress address>` |
| `*.<base>` | `A` (or `CNAME` / alias) | `<ingress address>` |

Find the address (ingress-nginx shown; use your controller's Service). If
the cluster had no ingress controller, run
`make prereqs INSTALL_CONTEXT="$CTX"` first so there is one to point at:

```sh
kubectl --context "$CTX" -n ingress-nginx get svc ingress-nginx-controller -o jsonpath='{.status.loadBalancer.ingress[0]}{"\n"}'
```

A hostname in that output means a `CNAME` (or an alias record on Route 53);
an IP means an `A` record. Per-cloud notes: section 2 (DNS & wildcard
certificate) of each `docs/cloud/*.md`.

These two records are all the DNS there is: `*.<base>` also matches every
site host, `<site>.<owner>.<base>`. Certificates for site hosts are per
owner; see "Site addresses" below.

### C. DNS-01 ClusterIssuer (only if none exists)

The certificate covers `<base>` and `*.<base>`, which needs DNS-01. If the
platform team owns cert-manager or DNS, ask them for a ClusterIssuer that
can solve DNS-01 for `<base>` and its name. Worked examples: section 2
(DNS & wildcard certificate) of `docs/cloud/aws.md` (Route 53), `gcp.md`
(Cloud DNS), `azure.md` (Azure DNS), and `oci.md`. Scope its DNS
credential to the one zone.

### Site addresses (owner certificates)

Every site is served at the root of its own host, `<site>.<owner>.<base>`
(for example `todo.alice.<base>`), so each site is its own browser origin.

- **DNS: nothing more.** The `*.<base>` record from step B already matches
  names at any depth, including `todo.alice.<base>`.
- **TLS: one certificate per owner.** A TLS wildcard covers exactly one
  label: the base certificate covers `alice.<base>` but not
  `todo.alice.<base>`. Each owner needs `*.<owner>.<base>`. With
  `OWNER_CERTS=auto` (the default), the component
  `deploy/components/owner-hosts`, already listed under `components:` in the
  byo `kustomization.yaml`, keeps one Ingress per owner with sites,
  annotated for cert-manager, and cert-manager issues that owner's
  certificate from the ClusterIssuer named in `OWNER_CERT_ISSUER`.
- **Sites keep serving at `<owner>.<base>/<site>/` until each owner's
  certificate is ready**, and responses give that address in `url` until
  then. Nothing breaks while a certificate is pending.

**Template annotations.** Every annotation on the install's own Ingress
(`OWNER_INGRESS_TEMPLATE`, default `simple-host`) except `cert-manager.io/*`
is copied onto each owner's Ingress, so keep base-host-only settings (a
`configuration-snippet`, an `auth-url`) off it.

**Ingress controller.** Automatic owner certificates are tested with an
in-cluster controller (ingress-nginx). AWS ALB (ACM certificates only, one
load balancer per Ingress without a shared `group.name`) and GKE's `gce`
Ingress (one load balancer per Ingress) do not fit one Ingress per owner:
run an in-cluster controller for the site hosts, or use `OWNER_CERTS=manual`.

Ask the human which ClusterIssuer signs owner certificates, then set
`OWNER_CERT_ISSUER=<its name>` in `config.env`:

- **An internal CA (recommended).** No rate limits. A cert-manager CA
  issuer is the smallest; Vault and Venafi issuers work the same way. The
  company's devices must trust the CA (managed devices usually already do).

  ```yaml
  apiVersion: cert-manager.io/v1
  kind: ClusterIssuer
  metadata:
    name: company-ca
  spec:
    ca:
      # A kubernetes.io/tls Secret in the cert-manager namespace holding the
      # CA's certificate and key.
      secretName: company-ca
  ```

- **ACME, such as Let's Encrypt.** Wildcards need DNS-01, so the DNS-01
  ClusterIssuer from step C works as it is. Let's Encrypt limits new
  certificates per registered domain per week, and this issues one per
  owner, so a large company hits the limit during rollout. Use an internal
  CA instead, or put the base domain on the Public Suffix List (each
  `<owner>.<base>` then counts as its own registered domain).

**Private registry.** The reconciler runs as its own ServiceAccount,
`simple-host-owner-hosts`. Add the same pull secret to it as to
`simple-host` (section 3), or it sits in `ImagePullBackOff` and every site
stays at its fallback address:

```yaml
patches:
  - target: {kind: ServiceAccount, name: simple-host-owner-hosts}
    patch: |
      - op: add
        path: /imagePullSecrets
        value: [{name: registry-pull}]
```

**Issuing owner certificates yourself.** If the platform team will not let
a pod create Ingresses, delete `- ../../components/owner-hosts` from
`components:` and set `OWNER_CERTS=manual` in `config.env`. Every owner is
then treated as ready, so each owner's certificate and ingress host rule
must exist before that owner publishes a first site. For owner `alice`
(copy the class and controller annotations from `ingress-patch.yaml`):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: sh-owner-alice-tls
  namespace: simple-host
spec:
  secretName: sh-owner-alice-tls
  dnsNames: ["*.alice.<base>"]
  issuerRef:
    kind: ClusterIssuer
    name: <issuer>
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: sh-owner-alice
  namespace: simple-host
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: 128m
spec:
  # ingressClassName: the same as ingress-patch.yaml, if set there
  tls:
    - hosts: ["*.alice.<base>"]
      secretName: sh-owner-alice-tls
  rules:
    - host: "*.alice.<base>"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: simple-host
                port:
                  name: http
```

Any other way of getting a `*.alice.<base>` certificate onto the ingress
works too. See `docs/site-isolation.md` for why each site has its own host.

## 7. Apply

```sh
make install OVERLAY=deploy/overlays/byo INSTALL_CONTEXT="$CTX"
```

Its `prereqs` step installs cert-manager if it is missing, and ingress-nginx
only when the cluster has no IngressClass at all; an existing controller is
used as it is, never joined by a second one. ingress-nginx is retired
upstream, so on a real cluster install the cloud's managed ingress first
(section 1). `INGRESS=nginx` installs
ingress-nginx regardless (then set `ingressClassName: nginx` in the
overlay); `INGRESS=none` never touches the controller.

Watch the migrate init container. It applies the schema and blocks the
rollout until it is current:

```sh
kubectl --context "$CTX" -n simple-host logs -l app=simple-host -c migrate --tail=50
```

```sh
kubectl --context "$CTX" -n simple-host rollout status deploy/simple-host --timeout=300s
```

If the rollout stalls, read the server's own log. Startup refusals name
every missing or invalid variable at once:

```sh
kubectl --context "$CTX" -n simple-host logs deploy/simple-host -c simple-host --tail=50
```

Wait for the certificate (the Ingress's `secretName` is `simple-host-tls`):

```sh
kubectl --context "$CTX" -n simple-host get certificate
```

`READY` must be `True`. If not, `kubectl describe` the certificate and its
`challenge` objects; the usual cause is the issuer's DNS credential.

Owner certificates appear as owners publish: after the first site, `kubectl --context "$CTX" -n simple-host get ingress,certificate` lists `sh-owner-<owner>` and `sh-owner-<owner>-tls`. Check the reconciler is running:

```sh
kubectl --context "$CTX" -n simple-host logs deploy/simple-host-owner-hosts --tail=50
```

## 8. Verify from outside

```sh
curl -fsS https://<base>/readyz
```

Expect `{"status":"ok"}`. Then check a person's host resolves and has a valid
certificate (any label works; `401` or `404` is fine, a TLS or DNS error is
not):

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://install-check.<base>/healthz
```

**HUMAN STEP D.** Ask an admin to:

1. Sign in at `https://<base>/auth/login`, then open `https://<base>/api/me`
   in the same browser and confirm it shows `"is_admin": true`.
2. On `/dashboard`, mint an API key named `install-check`.
3. Save it for you with this hidden prompt (stored outside the repository,
   readable only by them):

```sh
(umask 077; printf 'API key: '; read -rs v && printf '%s' "$v" > "$HOME/.simple-host-install-key" && echo " saved")
```

## 9. Smoke test against the real install

```sh
make smoke BASE=https://<base> KEY_FILE="$HOME/.simple-host-install-key"
```

This is `scripts/smoke-remote.sh`. It uses only public HTTPS and the admin's
key from HUMAN STEP D (read from the file, never printed): probes and TLS,
key auth, publish, update and roll back a throwaway `smoke-…` site, the
owner host, state read/write, an asset upload, list and delete, restricting
the site (its own host then refuses anyone it is not shared with), then deletes the site, on
failure too. Every line reads `ok` or `FAIL`; the exit status is the number
of failures. Without `BASE`, `make smoke` is the local overlay's test; do
not run that here.

Then the browser check it prints at the end: the admin opens
`https://<their label>.<base>/` and, after a few redirects, sees their own
index (the session hand-off, `docs/install.md` section 6). That proves the
wildcard certificate, DNS, and hand-off together.

Clean up: have the admin revoke the `install-check` key on `/dashboard`,
then:

```sh
rm -f "$HOME/.simple-host-install-key"
```

Last check: the prune CronJob runs the same image and fails silently if it
cannot pull it. Run it once with the dry-run preview in `docs/install.md`
section 8.

When everything passes, tell the human: the URL, who is admin, and that
people connect their agents through the plugin in `simple-host-plugin/`
against `https://<base>`.

## 10. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Pod exits at start with `missing required configuration: ...` | A required variable is unset. The message lists all of them. `docs/configuration.md`, and `docs/install.md` section 11. |
| Startup refuses the database TLS | `DB_SSLMODE` is not `verify-full`, or `db-ca.crt` is missing or the wrong CA. Re-download the provider's bundle. |
| Startup refuses the bucket endpoint | `BACKUP_STORAGE_ENDPOINT` is `http://`. Use `https://`. |
| Startup refuses `SESSION_SIGNING_KEY` | Wrong shape. It is `<id>:<base64 of 32 bytes>`, one or two comma-separated. Regenerate with section 4. |
| `BACKUP_STORAGE_ACCESS_KEY_ID and BACKUP_STORAGE_SECRET_ACCESS_KEY must be set together` | Set both or neither. |
| `OIDC_ADMIN_CLAIM` refused | Set together with `OIDC_ADMIN_VALUE`, or neither. |
| Migrate init container fails with a permission error | The owning role cannot create tables or roles. Use the cloud's admin user or grant `CREATEROLE`. |
| Migrate cannot connect | Network path from pods to the database (security group, authorised networks, private endpoint). Re-run the `pg_isready` check in section 2. |
| `schema version N is newer than this binary knows` | The image is older than the database's schema and a newer migration is not marked backward-compatible. Roll forward, or restore with PITR; `docs/install.md` section 10. |
| Pod crash-loops with `discover OIDC provider` | The issuer is unreachable from the pod, or `OIDC_ISSUER` is wrong. Run the in-cluster issuer check in section 1. |
| `/readyz` returns 503 | Database not reachable, or schema not current. Read the `migrate` and `simple-host` container logs. A bucket fault does not fail `/readyz`: look for `readyz: bucket:` in the logs and `simplehost_bucket_ok 0` on `/metrics`. |
| Sign-in: "the identity provider did not send an email address" | Provider does not put `email` in the ID token. Add the claim, or set `OIDC_EMAIL_CLAIM` (Entra: see HUMAN STEP A). |
| Sign-in: "this account's email domain is not allowed" | `ALLOWED_EMAIL_DOMAINS` does not include the person's domain. |
| Sign-in: "your email address is not verified" | The ID token has no `email_verified: true`. Entra: add the `xms_edov` optional claim (HUMAN STEP A). |
| Server refuses to start: "can alter or delete audit_events" | The server is connected as the owning role. Keep the manifest's `DB_APP_USER=simplehost_app`, and do not point `DB_DSN` at the owning role. |
| Sign-in fails at the callback with a redirect-URI error | The registered redirect URI is not exactly `https://<base>/auth/callback`. |
| Sign-in fails at token exchange | Wrong client secret, or the provider does not accept `client_secret_post` (Okta: HUMAN STEP A). Re-enter with section 4's hidden prompt, re-apply, then `kubectl --context "$CTX" -n simple-host rollout restart deploy/simple-host`. |
| Signed-in admin does not see `/admin` | Their address is not in `ADMIN_EMAILS` (compared lowercased), or the admin claim does not match. Takes effect at the next sign-in; removal from `ADMIN_EMAILS` also takes effect at the next server restart. |
| Upload of a site fails with 413 | The ingress body-size limit. Section 5 step 3. |
| Certificate never becomes ready | DNS-01 solver credential or zone. `kubectl describe` the `certificate`, `order`, and `challenge`. |
| Sites stay at `<owner>.<base>/<site>/` | The owner's certificate is not ready. Read the `simple-host-owner-hosts` logs, check `OWNER_CERT_ISSUER` names a ClusterIssuer that exists, and `kubectl describe certificate sh-owner-<owner>-tls`. Section 6, "Site addresses". |
| CronJob in `ImagePullBackOff` | Pull secret on the Deployment instead of the ServiceAccount. `docs/install.md` section 10. |
| `429` or "gave no redirect" after many sign-ins | `/auth/*` is rate limited per address. Wait two to three minutes. |

Anything not listed: `docs/install.md` section 11, then the refusal column
of `docs/configuration.md`.

## 11. After the install

- **Upgrade from v1.0.x**: sites move from the volume to the bucket, so
  follow `docs/storage.md`, "Migrating from a PVC install", instead of a
  plain apply. It includes the database backup and the step that applies
  the migrations.
- **Upgrade**: `docs/install.md` section 10. Pick the release from
  `CHANGELOG.md`, resolve and verify its digest (section 3), update
  `images:` in the overlay, apply, watch the rollout. The startup log line
  (or `/simple-host version`) names the running release and schema.
  Roll back with `kubectl --context "$CTX" -n simple-host rollout undo deploy/simple-host`;
  if the server refuses the newer schema, roll forward or restore the
  database with PITR to before the upgrade (`docs/install.md` section 10).
- **Backups**: `docs/install.md` section 9 and `docs/storage.md`. Sites live
  in the bucket and are recovered through its versioning. Nothing in this
  package backs up Postgres, which holds the per-site state, accounts and
  audit trail; the managed database's PITR is its whole backup. Confirm PITR
  retention with the human.
- **Monitoring**: `docs/install.md` section 12. Metrics are on pod port 9090
  (`/metrics`), not on the Service or Ingress; it lists what to watch.
- **Config changes**: edit `config.env` or `secrets.env`, re-apply, then
  `kubectl --context "$CTX" -n simple-host rollout restart deploy/simple-host`.
- **Secret rotation**: `SESSION_SIGNING_KEY` rotates by adding a second
  key (`docs/configuration.md`). `BACKUP_ENVELOPE_KEY` rotates by putting the
  new key first, deploying, running `simple-host reencrypt`, then removing
  the old key (`docs/storage.md`, "Rotating the envelope key"). Entra client
  secrets expire; note the date for the human.
- **Audit**: every committed audit event is also one JSON line on the pod's
  stdout with `"type":"audit"` and its hash-chain `seq` and `hash`; point the cluster's log shipper at it to feed a
  SIEM (`docs/configuration.md`, "Streaming the audit log to a SIEM").
  `simple-host audit-verify` checks the audit hash chain
  (`docs/security-review.md`, 2(e)).
- **Uploads**: per-owner quotas default to 1000 sites and 10 GiB; set
  `CLAMD_ADDR` to scan uploads with clamd (`docs/configuration.md`).
- **Keep** `deploy/overlays/byo/config.env`, `secrets.env`, and `db-ca.crt`
  somewhere the platform team can find them (their secret store), or move
  to External Secrets as `docs/configuration.md` describes. They are not in
  git by design.
