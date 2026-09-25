# Installing Simple Host

Reconciled against Phase 7's cold, start-to-finish run (`docs/security-review.md section 3`):
sections 1-2 (Docker Desktop) were followed literally by a stranger, from a
`make local-down` teardown, with no guide defect found. Sections 3-9 (minikube,
Google sign-in, a real cluster, and everything past first bring-up) are
unchanged by that run and are not themselves re-verified here.

This guide takes a platform team from nothing to a signed-in dashboard,
first on a local cluster, then on a real one with your own Postgres and
bucket. Every command below is a `make` target or a `kubectl`/`kustomize`
invocation that exists in this repository today.

As of this draft, Phases 0 (bootstrap and packaging), 1 (identity), 2
(subdomain-only serving and restricted sites), 3 (the site-facing state and
asset API), 4 (the action and access audit), and 5 (data protection) are
built and live-verified against the local cluster. Only Phase 7 (the paced,
end-to-end pen-test run) remains before this guide is fully reconciled; see
`docs/security-review.md` for what that still needs.

## 1. Before you start

You need, on the machine that will run the install:

- `docker`, `kubectl`, `kustomize`, `mkcert` — all four are checked by
  `make local-tools`.
- Docker Desktop with Kubernetes enabled (the primary target), or minikube
  with the `docker` driver (the alternate — see the callout in step 3).
- Go 1.25, only if you intend to build the binary yourself rather than use
  a pre-built image; `make build` and `make image` both do this for you.

You do **not** need a cloud account, a domain you control, or an existing
Postgres or bucket to complete the local-cluster walkthrough. Those come in
section 5, for a real cluster.

## 2. Local cluster: Docker Desktop (primary)

Enable Kubernetes in Docker Desktop's settings and confirm `kubectl
--context docker-desktop cluster-info` succeeds before continuing.

```sh
make local
```

This one target runs, in order (see the `Makefile` for the exact steps if
you want to run them individually):

1. `local-tools` — checks `docker`, `kubectl`, `kustomize`, `mkcert` are on
   your `PATH` and that the `docker-desktop` context is reachable.
2. `local-third-party` — applies pinned releases of ingress-nginx and
   cert-manager, and waits for both to be ready. These are applied outside
   the kustomize overlay on purpose: the overlay's `Certificate` objects
   need cert-manager's CRDs to exist first.
3. `local-certs` — runs `mkcert -install` (see the interactive-step
   callout below), then issues a certificate for
   `simple-host.127-0-0-1.nip.io` and its wildcard, and loads it as a TLS
   Secret in the `simple-host` namespace.
4. `local-secrets` — writes `deploy/overlays/local/secrets.env` with
   freshly generated random values, if it does not already exist. This
   file is gitignored; it is never committed.
5. `image` and `local-image-load` — builds the container image and copies
   it into the Docker Desktop node's image store (Docker Desktop's
   Kubernetes has its own store, separate from your local `docker images`).
6. `local-up` — applies `deploy/overlays/local` and waits for the
   Postgres, MinIO, Dex, and application rollouts to become ready.

Cold bring-up takes under five minutes on a 4-CPU/6-GiB Docker Desktop VM,
most of it image pulls.

**The one interactive step.** `mkcert -install` asks for your macOS
keychain password to add its root certificate to the system trust store.
There is no way around this in an unattended run; if you are scripting
`make local` non-interactively, run `mkcert -install` once by hand first
and it will be a no-op on subsequent runs. Certificates issue from the
mkcert CA regardless of whether `-install` has completed — only your
browser's trust of the site depends on it. `make smoke` never depends on
system trust: it points curl at the mkcert root directly through
`CURL_CA_BUNDLE`.

When `make local` finishes, it prints the URL and two test accounts:

```
Simple Host is up at https://simple-host.127-0-0-1.nip.io
Sign in at https://simple-host.127-0-0-1.nip.io/auth/login as admin@example.com / adminpass
or person@example.com / personpass (Dex's two static test accounts;
see deploy/components/dex/configmap.yaml), then mint a key from /dashboard.
```

Dex is the identity provider for the local overlay and for CI; it is not
meant to be a real one. Section 4 below walks the same sign-in with Google
instead, which is what the local overlay's manual rehearsal and any real
company install use.

Verify:

```sh
curl --cacert "$(mkcert -CAROOT)/rootCA.pem" https://simple-host.127-0-0-1.nip.io/readyz
```

expect `{"status":"ok"}`. Then open
`https://simple-host.127-0-0-1.nip.io/auth/login` in a browser, sign in as
`admin@example.com` / `adminpass`, and confirm `/dashboard` loads with a
key-minting form and `/admin` shows the admin view.

Run the end-to-end check:

```sh
make smoke
```

`scripts/smoke.sh` signs in through Dex as both test accounts, mints and
revokes an API key, confirms `X-API-Key` and the session cookie both
authenticate `/api/sites`, confirms the old `/api/auth` and
`/api/reset-requests` registration routes are gone (404), drives the full
hand-off round trip from the base host to an owner host's own session,
restricts a site to a named viewer and confirms it serves only on its own
`<owner>--<site>` hostname (404 to everyone else), checks the
`Cross-Origin-Resource-Policy` and `Sec-Fetch-Site` refusal headers, reads
and writes a site's state and uploads/lists/deletes an asset through the
site-facing API by both session and `X-API-Key`, and confirms an
`_assets/`-containing archive and an HTML file relabeled as an image are
both refused. It also confirms a real `audit_events`/`access_log` row lands
in Postgres for the run's writes and views, reads them back through
`GET /api/audit`/`GET /api/access` as both the owner and a stranger, and
exercises `GET /api/admin/export` and `simple-host prune -dry-run` — see
section 8.

Tear down with `make local-down`, which deletes everything the overlay
created, the Postgres and MinIO volumes included — local data is disposable by design.

## 3. Local cluster: minikube (alternate)

minikube with the `docker` driver works the same way, with two differences:

```sh
minikube start --driver=docker
minikube addons enable ingress
make local CLUSTER_CONTEXT=minikube
```

- On macOS, minikube's `docker` driver runs the cluster inside a VM, so its
  `ingress` addon does not bind to your host's loopback the way Docker
  Desktop's does. Run `minikube tunnel` in a separate terminal and leave it
  running for the duration of your session; without it,
  `https://simple-host.127-0-0-1.nip.io` will not reach the cluster even
  though every pod is healthy.
- `local-image-load` uses `minikube image load` instead of the Docker
  Desktop node's debug-pod path (`scripts/load-image.sh`) when
  `CLUSTER_CONTEXT=minikube`.

Every other step, and `make smoke CLUSTER_CONTEXT=minikube`, is identical.

## 4. Google as the identity provider (manual rehearsal)

Dex proves the sign-in code path in CI, but every real install — and the
one manual rehearsal this package's own development runs before trusting a
release — uses a real provider. Google is documented in full because it is
the provider named in the design (10.7) and the one with the sharpest
edges (a redirect URI on `.localhost` or a raw IP is refused outright).
Any OIDC provider works the same way with different console screens; see
`docs/configuration.md` for the generic `OIDC_*` variables.

1. In Google Cloud console, on the project of your choice: **APIs &
   Services → OAuth consent screen**. Choose **Internal** if your
   organization is on Google Workspace — only your organization's accounts
   can sign in, and no app verification is required. Choose **External**
   only for a rehearsal without Workspace, and list every tester as a test
   user.
2. **Credentials → Create credentials → OAuth client ID**, type **Web
   application**.
3. **Authorised JavaScript origins:** `https://<base>`. **Authorised
   redirect URI:** `https://<base>/auth/callback`, exactly — Google matches
   it byte-for-byte. For the local overlay that is
   `https://simple-host.127-0-0-1.nip.io/auth/callback`.
   `https://simple-host.localhost/auth/callback` is refused ("must end
   with a public top-level domain"), and so is a raw IP address. This is
   exactly why the local overlay's base host is
   `simple-host.127-0-0-1.nip.io` and not `.localhost` — see design 10.6.
4. Copy the client ID and secret into `secrets.env` as `OIDC_CLIENT_ID` and
   `OIDC_CLIENT_SECRET`. Set `OIDC_ISSUER=https://accounts.google.com` in
   `config.env`. Leave `OIDC_SCOPES` at its default,
   `openid email profile` — Google has no groups scope for the ID token,
   so do not add one.
5. Set `ALLOWED_EMAIL_DOMAINS=<your Workspace domain>`. The server sends it
   as the `hd` hint on the authorization request and checks the `hd` claim
   and the email domain on the callback; a consumer Google account is
   refused. With **Internal** chosen in step 1, Google enforces the same
   boundary on its own side, so the two agree.
6. Set `ADMIN_EMAILS=<your platform team's addresses>`. This is the only
   admin control for Google: nothing in its ID token can substitute for it.
7. On the local overlay, remove the `dex` component from
   `deploy/overlays/local/kustomization.yaml` (or simply override the three
   `OIDC_*` values with Google's), redeploy, and sign in once as an admin
   address and once as a non-admin address. Confirm the dashboard shows the
   admin section to the first and not the second.

## 5. A real cluster with your own Postgres and bucket

This is `deploy/overlays/byo` — no database or bucket objects are deployed;
the overlay carries only the application, pointed at coordinates you
supply.

1. Pick the image. The recommended one is the published release,
   `ghcr.io/vineetu/simple-host-enterprise:v1.0.0`, pinned by its digest
   `sha256:8991d5e9fc8c33e69981c4fa25ca5b3f5964f4087ed10c1074d7016623939489`.
   Or push the image you built (`make image`, or your own CI build of the
   same `Dockerfile`) to a registry your cluster can pull from, scan it,
   and resolve its immutable digest.
2. Copy the example files and fill them in:

   ```sh
   cp deploy/overlays/byo/config.env.example deploy/overlays/byo/config.env
   cp deploy/overlays/byo/secrets.env.example deploy/overlays/byo/secrets.env
   ```

   `config.env` needs your base URL, your OIDC provider's issuer and client
   ID, your admin email list and allowed domain(s), your database host and
   name, and your bucket's endpoint, region, bucket name, and prefix (turn
   on the bucket's versioning first: `docs/storage.md`).
   `secrets.env` needs the OIDC client secret, the session signing key, the
   database owner and application-role passwords, and (if your bucket does
   not use workload-identity credentials) an access key pair. Both files
   are gitignored once created; `docs/configuration.md` documents every
   variable in both.
3. Place your managed database's CA bundle at
   `deploy/overlays/byo/db-ca.crt` — there is no `.example` for this file,
   since it is provider-specific and not a value you type in. The startup
   check refuses `sslmode=verify-full` (the only mode this package accepts
   outside `DB_INSECURE_ALLOWED=true`) without a root certificate to verify
   the server against.
4. Edit `deploy/overlays/byo/ingress-patch.yaml`: set your real hostnames
   in place of `simple-host.example.com` and `*.simple-host.example.com`,
   and name the `ClusterIssuer` your platform team configured for DNS-01
   wildcard issuance in place of `letsencrypt-dns` (see `docs/cloud/` for a
   worked example per cloud). `ingressClassName` is left unset there so
   the cluster's default IngressClass serves it; set it only to pick a
   non-default class. `make install` adds ingress-nginx only when the
   cluster has no IngressClass at all (`INGRESS=nginx` forces it,
   `INGRESS=none` never touches the controller), so an existing ALB, GKE,
   Traefik or AGIC controller is never joined by a second one.
5. Edit `deploy/overlays/byo/kustomization.yaml`'s `images:` entry to point
   at the image and digest from step 1 (`newName` and `digest`), replacing
   `sha256:REPLACE_WITH_THE_SCANNED_IMAGE_DIGEST`.
6. Point a wildcard DNS record at your ingress controller's address for
   both `<base>` and `*.<base>`.
7. Apply:

   ```sh
   kustomize build deploy/overlays/byo | kubectl apply -f -
   kubectl -n simple-host rollout status deploy/simple-host
   ```

   The migrate init container runs first and blocks the rollout until the
   schema is current; watch its logs (`kubectl -n simple-host logs
   -l app=simple-host -c migrate`) if the rollout stalls.
8. Follow section 4 above for your OIDC provider, using your real base URL
   in the redirect URI.
9. `curl https://<base>/readyz` should return `{"status":"ok"}`; sign in at
   `https://<base>/auth/login` as one of your `ADMIN_EMAILS` addresses.
10. Mint an API key on `/dashboard`, save it to a file, and run
    `make smoke BASE=https://<base> KEY_FILE=<that file>`. It publishes,
    restricts and deletes a throwaway site over public HTTPS only; the key
    is never printed. Revoke the key afterwards.

`deploy/overlays/staging` and `deploy/overlays/production` are templates on
top of the same `byo` shape, for a company that wants a separate
environment before the one it puts employees on; they copy `byo`'s
`.example` files today and are not further along.

## 6. Sign-in, hand-off, and restricted sites

Every hosted page and every site-facing API call requires a signed-in host
session — there is no anonymous viewing and no Referer-based attribution
anywhere in this package. Signing in on the base host does not, by itself,
authenticate you on `alice.<base>`: the first time you open a page on an
owner host, the server sets a short-lived nonce cookie on that host,
redirects you to `<base>/auth/handoff` to mint a one-time code bound to
that nonce, and redirects you back to redeem it and mint that host's own
`__Host-sh_session` cookie. This is transparent in a browser (it is one
extra redirect hop on first visit to each host) and is exactly what
`scripts/smoke.sh`'s `hand_off_session` helper drives end to end with curl
and a cookie jar, if you want to see the wire protocol.

A site is open to any signed-in person by default. An owner restricts it
to a named list of viewers from `/dashboard`'s "Your sites" panel (add or
remove usernames under a site's "Viewers" section) or through the API
(`GET`/`POST /api/collaboration/sites/{owner}/{sitename}/viewers`,
`DELETE .../viewers/{username}`). The moment a site has at least one named
viewer, it moves to its own dedicated hostname,
`<owner>--<site>.simple-host.127-0-0-1.nip.io` on the local overlay, and
stops being reachable at the ordinary `alice.<base>/<site>/` address at
all — a non-listed signed-in person gets `404`, not `403`, so the site's
existence is never confirmed to someone it isn't shown to. The local
overlay's wildcard certificate, `*.simple-host.127-0-0-1.nip.io`, already
covers this address with no extra step: `<owner>--<site>` is one DNS label
(it contains a hyphen, not a dot), so `mkcert`'s existing wildcard from
section 2 matches it exactly the same way it matches an ordinary owner
label. The same is true of your own wildcard on a real cluster.

One naming rule: a label whose third and fourth characters are `--` is a
reserved LDH label under RFC 5890 and some validators refuse it, so the
`--` address is only offered when the *owner's* label is at least three
characters (rare to hit in practice, since the label derives from an
email's local part); a two-character owner sees an explanation in the
dashboard rather than the option, and a site name that itself does not
fold into a valid DNS label has no working restricted address yet either
(a known gap — see `docs/security-review.md`'s Remains).

## 7. Reading and writing a site's own state and assets

Built and verified (Phase 3). A page's own JavaScript calls
`GET`/`PUT /api/sites/{site}/state` (last-write-wins) or
`GET`/`PUT /api/sites/{site}/state/versioned` (compare-and-set; the
default for a new stateful site) on its own host, authenticated by the
viewer's session cookie exactly like viewing the page itself — there is no
separate token to manage. `{site}` is computed from the page's own address
with `location.pathname.split('/')[1]` on an ordinary owner host; a
restricted site's own host has no `/<site>/` segment, so a page that might
be restricted should call the nameless `/api/site/state...` shape instead
or bake its site name in at deploy time, per
`simple-host-plugin/skills/simple-host/references/state-and-ai.md`. A
`401` means the session expired or the page moved hosts; the fix is
`location.reload()`, which re-enters the hand-off above, not a retry with
the same credentials. An agent calling these routes directly with `curl`
and `X-API-Key` skips the browser `Origin` check entirely, which is how an
agent seeds or reads a site's state with no browser involved.

Assets — a photo, PDF, audio/video clip, or a CSV/JSON/plain-text/zip/gzip
file a page wants to reference by URL, outside the site's own version
history — use `POST`/`GET /api/sites/{site}/assets`,
`DELETE /api/sites/{site}/assets/{id}`, and
`GET /{site}/_assets/{id}[/{name}]` on the same host and the same session.
The upload is refused (`415`) whenever the file's sniffed bytes don't
match the allowlist (`image/*`, `video/*`, `audio/*`, `application/pdf`,
JSON, CSV, plain text, zip, gzip) regardless of its declared type or
extension — a relabeled HTML file is refused, never served. Default
limits are `ASSET_MAX_FILE_BYTES`/`ASSET_MAX_SITE_BYTES`/
`ASSET_MAX_SITE_COUNT` (`docs/configuration.md`: 25 MiB / 500 MiB / 5,000).
An owner also manages a site's assets from `/dashboard`'s "Your sites"
panel, or through the base-host, session-authenticated mirror
`GET`/`DELETE /api/collaboration/sites/{owner}/{sitename}/assets[/{id}]`.
A site archive may not contain a top-level `_assets/` entry; the upload is
refused before it ever reaches disk.

Every state and asset write is attributed to the viewer and the site it
was made through (`via_site`); this is exactly what the `state_write`,
`asset_create`, and `asset_delete` audit rows in section 8 below record,
including the distinction between the claimed site and the one the browser
actually corroborated (`via_site_observed`).

## 8. The audit trail and access log

Built and verified (Phase 4). Every mutation writes an `audit_events` row.
Sixteen actions write it inside the same database transaction as the
change it records (`internal/audit`'s `RecordTx`), so a mutation in this
group without its audit row cannot commit: `site_create`, `site_update`,
`site_delete`, `site_rollback`, `site_visibility`, `editor_grant`,
`editor_revoke`, `viewer_grant`, `viewer_revoke`, `team_create`,
`team_delete`, `member_add`, `member_remove`, `state_write`,
`asset_create`, and `asset_delete` — the last three from the site-facing
API (`internal/handler/site_api.go`'s `PutState`/`PutStateVersioned`/
`CreateAsset`/`DeleteAsset`) and, for `asset_delete`, also from the
dashboard's base-host mirror (`internal/handler/assets_admin.go`'s
`deleteCollaborationAsset`) — both call sites now share this guarantee.
For both asset-delete call sites, the file on disk is deliberately kept
outside that transaction: an orphaned or already-removed file from a
partial failure is recoverable, while the database row and its audit row
now commit or roll back together. Eleven actions remain best-effort,
written through the older `Record` with no shared transaction: `sign_in`,
`sign_out`, `session_revoke`, `key_mint`, `key_revoke`, `hand_off`, and the
admin actions `admin_disable_user`, `admin_enable_user`,
`admin_archive_versions`, `admin_classify_sites`, `admin_export`. See
`docs/security-review.md`'s S9a row for the same inventory.
`state_write` coalesces repeated writes from the same actor
and site into one row per five-minute window (`detail.count`), so autosave
at page frequency does not bury every other action; every other action is
one row each. Separately, `access_log` records every view of hosted
content, including the owner's and editors' own — the `isSelfTraffic`
exclusion in `serve.go` only ever applied to the dashboard's analytics
counters, never to this log.

Read it back:

```
GET /api/audit?owner=&site=&actor=&action=&from=&to=&cursor=
GET /api/access?owner=&site=&from=&to=&cursor=
```

`owner`/`site`/`actor` on `/api/audit` are usernames and a site name, never
raw ids — the server resolves them for you, and an unresolvable name comes
back as an empty page, not a `404`, since a filter that matches nothing is
not the same claim as a request that failed. A non-admin's results are
scoped to their own account plus every team they belong to; an admin may
query any owner. `/api/access` needs an explicit `owner=` from a non-admin
caller (`400` without one) and redacts `ip`/`user_agent` from a non-admin's
own results — only an admin sees those two fields. `ACCESS_LOG_VISIBILITY`
(`docs/configuration.md`, default `owner`) can be set to `admin` to close
`/api/access` to non-admins entirely, regardless of ownership.

The dashboard's per-site panel gained "Activity" and "Visitors" tabs
(`/dashboard`, under "Your sites") backed by the same two routes scoped to
that site; the admin view gained an "Activity & visitors" section
(`/admin#admin-activity`) with the same two feeds unfiltered across every
owner, plus an Export CSV/JSONL link per feed to:

```
GET /api/admin/export?kind=audit|access&from=&to=&format=csv|jsonl
```

Admin only, streamed one page at a time rather than buffered whole, and
itself recorded as an `admin_export` audit row before the body starts.
Unlike the admin dashboard's mutating routes, this one is not wrapped in
the dashboard's Origin check — deliberately: it is a side-effect-free
`GET` with no CORS headers anywhere in this package, so a page on another
origin can trigger the request but cannot read its response body back.
See `docs/security-review.md`'s accepted limitations for the same note.

Retention runs as a monthly `CronJob` (`simple-host-prune`, wired into
`deploy/base/kustomization.yaml`), which advances the rolling monthly
partitions and then drops any whose partition is fully past its retention
window — `AUDIT_RETENTION_DAYS` (default 400) and
`ACCESS_LOG_RETENTION_DAYS` (default 90), both in `docs/configuration.md`.
It runs under the database's owning credential, not `simplehost_app`: the
application role has no `DROP` privilege on either table at all (design
9.3), so retention can only ever run as the owning role. Preview what a
run would drop without dropping anything:

```sh
kubectl -n simple-host create job simple-host-prune-preview \
  --from=cronjob/simple-host-prune --dry-run=client -o json \
  | python3 -c "import json,sys; j=json.load(sys.stdin); j['spec']['template']['spec']['containers'][0]['args']=['prune','-dry-run']; print(json.dumps(j))" \
  | kubectl -n simple-host create -f -
kubectl -n simple-host wait --for=condition=complete job/simple-host-prune-preview --timeout=60s
kubectl -n simple-host logs job/simple-host-prune-preview
kubectl -n simple-host delete job simple-host-prune-preview
```

`kubectl create job --from=cronjob/... | kubectl patch` does not work here:
a Job's pod template is immutable once the object exists, so the `-dry-run`
flag has to be patched into the rendered JSON before it is ever created.
`scripts/smoke.sh` does exactly this.

Two known gaps, both flagged rather than silently left: `/api/audit`'s
`actor_id`/`owner_id`/`site_id` fields are not resolved back to
usernames/site names, so the dashboard's Activity tab shows raw ids for
those columns (the action, timestamp, and `via_site_label`/`via_site_name`
carry most of the practical signal); and `admin_disable_user`/
`admin_enable_user` still audit with a plain, non-transactional write
(`db.SetUserDisabled` opens its own internal transaction with no way for
the caller to share it), unlike every other action above. Two actions design
8.1 names — `admin_transfer_sites` and `site_write_mode` — have no route
anywhere in this repository to audit yet: neither a site-transfer feature
nor a per-site write-mode setting has been built by any phase, so there is
nothing to wire until one exists.

## 9. Backup and restore

The bucket is the site store, not a copy of it: every version and asset is
written there with a server-side-encryption header (`BACKUP_SSE`, default
`AES256`); set `BACKUP_ENVELOPE_KEY` for an additional client-side envelope
that makes the objects unreadable to anyone who can read the bucket but not
your Kubernetes Secrets. Recovery comes from bucket versioning, which must be
on. `docs/storage.md` covers the bucket setup, retention, the `restore`
subcommand, and migrating an install that still keeps sites on a volume.

**If your bucket is MinIO**, it refuses any `PutObject` carrying a
server-side-encryption header at all — including the default `AES256` —
unless it has a KMS backend configured. The local overlay sets
`MINIO_KMS_SECRET_KEY` on the MinIO StatefulSet for exactly this reason
(MinIO's own single-key auto-encryption backend, no external KES needed).
This is a property of self-hosted MinIO, not of Simple Host or of
S3-compatible stores generally; a managed bucket (S3, Cloud Storage, Azure
Blob through a compatible front) needs no equivalent step.

The state document itself — the per-site JSON store — lives only in
Postgres, so its backup is your Postgres backup and PITR policy, not
anything this package copies to the bucket. Set your managed Postgres's
PITR retention deliberately; see `docs/cloud/` for the per-cloud knob.

## 10. Upgrade

A deploy is: build the image in CI, scan it, push it by digest, update the
overlay's `images:` entry (or your CI's equivalent), `kubectl apply` the
rendered overlay, and watch the rollout. Rollback is the previous digest —
**only** under expand/contract migration discipline: a release may add
columns and tables, and may stop reading a column, but drops it one
release later, never in the same release it stops using it.

The binary itself enforces the forward half of this: at startup, it refuses
to run against a `schema_migrations` table that holds a version newer than
the newest migration it embeds, failing at startup with the version
mismatch named rather than failing confusingly at the first query that
touches a column it does not expect. This means an old image can never be
rolled back onto a schema a newer release has already contracted — if a
a migration is one-way, its rollback path is "restore the previous schema
from your Postgres PITR window," not "redeploy the old image."

A migration that drops or rewrites a column is one-way. Read the files added
since the version you are on, in `internal/migrate/sql/`, before planning a
rollback — they are numbered and each one says what it does.

### If you use a private registry

Put the pull secret on the **ServiceAccount**, not the Deployment. Two
workloads run this image: the server and the `prune` CronJob. Kubernetes
applies a ServiceAccount's `imagePullSecrets` to every pod that uses it, so
one patch covers both.

Patching only the Deployment leaves the CronJob in `ImagePullBackOff`. The
instance serves normally, so nothing looks wrong — but no audit partition is
ever dropped. This does not show up on a local
cluster, where the image is loaded into the node and never pulled at all.


## 11. Troubleshooting

- **`mkcert -install` prompts for a password and the run is not
  interactive.** Expected — see the callout in section 2. Run it once by
  hand, or issue certificates without installing the root (curl-only
  verification via `CURL_CA_BUNDLE`, as `make smoke` does).
- **A rollout on minikube never reaches the host.** You are missing
  `minikube tunnel`; see section 3.
- **`kustomize build deploy/overlays/local | kubeconform -strict` reports
  errors about `Certificate`/`Issuer` objects having no schema.** This is
  a `kubeconform` limitation, not a real problem: it has no CRD catalogue
  for cert-manager's types without one supplied. Use `-ignore-missing-schemas`,
  or supply cert-manager's CRD schemas; `byo`, which defines no
  cert-manager objects of its own, passes plain `-strict` unmodified.
- **`byo`, `staging`, or `production` fail to `kustomize build` at all.**
  They need `config.env`, `secrets.env`, and `db-ca.crt` copied and filled
  in before they render (section 5, steps 2-3); there is no default that
  lets them build empty, deliberately.
- **A `PutObject` to your bucket is refused with `501 NotImplemented:
  Server side encryption specified but KMS is not configured`.** You are
  running self-hosted MinIO without a KMS backend; see section 9's MinIO
  callout.
- **Startup fails with a list of missing configuration.** This is by
  design (`docs/configuration.md`): nothing that identifies your
  installation has a default, so a misconfigured pod fails at startup
  naming every gap at once rather than silently reaching someone else's
  infrastructure.
- **Startup refuses your database or bucket TLS.** `sslmode` must be
  `verify-full` with a root certificate, and the bucket endpoint must be
  `https://`, unless you have deliberately set `DB_INSECURE_ALLOWED=true`
  or `BACKUP_STORAGE_INSECURE_ALLOWED=true` for a local evaluation cluster —
  never set either on a real install.
- **You are standing up your own OIDC provider for CI (a second Dex, a
  test Okta tenant, and so on) and the server cannot reach it.** Every OIDC
  call this server makes — discovery, token exchange, JWKS — runs
  pod-to-provider, not browser-to-provider. If your provider's
  browser-facing hostname is not reachable from inside the cluster (as
  Dex's public nip.io hostname is not, from a pod, on the local overlay),
  point `OIDC_ISSUER` at the provider's in-cluster Service DNS name
  instead, and reach its login form yourself for manual testing through a
  `kubectl port-forward` with `curl --resolve` so the `Host` header still
  matches the issuer. `deploy/components/dex/configmap.yaml` and
  `scripts/smoke.sh` show the exact mechanism this package uses for Dex.
- **A person's account gets a `-2` (or higher) suffix on their username at
  first sign-in.** Expected when their derived username collides with an
  existing account or a reserved label (`admin`, `www`, and so on) —
  common for an admin whose real address is literally
  `admin@yourcompany.com`. The dashboard shows a one-time notice; this is
  not an error.
- **`make smoke` or a manual pen-test run starts failing with `429`s, or
  "gave no redirect to the identity provider," after several back-to-back
  runs from the same machine in a short window.** Not a regression:
  `/auth/*` is rate-limited by peer address (Burst 20, refill 0.2/s), and
  each sign-in-driven check in the smoke and pen-test suites uses several
  requests against it. Wait two to three minutes between runs, or space
  out individual `go test -run` invocations.
- **`/readyz` reports `503` forever after you change a migration that
  drops a column or trigger.** The readiness probe
  (`internal/handler/health.go`) checks for specific schema objects by
  name; if a migration you add removes one the probe still expects, update
  the probe's expectation in the same change, or every binary built after
  that migration lands will never become Ready. This bit the local overlay
  once during development (migration `0025` dropped a trigger the probe
  still checked for) — the fix is recorded in
  `docs/security-review.md`'s Live verification section as a
  worked example.

## Not yet covered here

- The full pen-test list (design section 12), which Phase 7 runs against
  a live local overlay; `test/pentest` already covers Phases 1-4's items
  in isolation (build tag `pentest`; see that package's own doc comment
  for the environment variables it needs) but has not yet been run as one
  paced CI suite.
- ~~A cold, start-to-finish run of this guide on a genuinely fresh
  cluster, by someone with no other context~~ — done in Phase 7: `make
  local-down` then every command in sections 1-2 literally, on a stranger's
  first run, reached a signed-in dashboard with no guide defect found; see
  `docs/security-review.md section 3`. The one interactive step
  (`mkcert -install`'s keychain password) has no non-interactive
  workaround; this run used the section-2 callout's own alternative
  (issue certificates without installing the root) rather than a guide fix.
