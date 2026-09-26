# Installing Simple Host

This guide takes a platform team from nothing to a signed-in dashboard,
first on a local cluster, then on a real one with your own Postgres and
bucket. Every command below is a `make` target or a `kubectl`/`kustomize`
invocation that exists in this repository today.

Sections 1-2 (Docker Desktop) have been followed start to finish from a
clean teardown. `docs/cloud/` records which managed-cloud guides have been
verified by a real install.

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
authenticate `/api/sites`, drives the full
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
a common choice and the one with the sharpest
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
   `simple-host.127-0-0-1.nip.io` and not `.localhost`.
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

Choose `<base>` on a registrable domain of its own, such as
`corp-sites.com`, never a subdomain of the company's main domain such as
`simple-host.corp.com`: hosted pages are written by anyone in the company,
and on the main domain they would be same-site with its other apps, able to
receive and plant cookies scoped to that domain. Set `TRUSTED_PROXY_CIDRS`
to the range your ingress controller's pods take their addresses from
(`kubectl -n <ingress-namespace> get pod -o wide` shows them; a node's
`.spec.podCIDR` is not always that range) so rate limits and logs see each
person's address rather than the ingress's (`docs/configuration.md`).

1. Pick the image. The recommended one is the published release,
   `ghcr.io/vineetu/simple-host-enterprise:v1.1.3`, pinned by its digest
   `sha256:c7406f35919b1300d8f938f0476e92a00586b1c327d8de20fb6a0dd209d42826`.
   Or push the image you built (`make image`, or your own CI build of the
   same `Dockerfile`) to a registry your cluster can pull from, scan it,
   and resolve its immutable digest. Verify a release image's signature and
   read its SBOM before pinning it (`INSTALL.md` section 3).
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
   variable in both. Pods do not restart when these files change: after
   re-applying an edit, run `kubectl -n simple-host rollout restart deploy/simple-host`.
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
   ingress-nginx is retired upstream: best-effort maintenance ended in March
   2026, with no further releases or security fixes. On a real cluster,
   prefer the cloud's managed ingress (AWS Load Balancer Controller, GKE
   Ingress or Gateway, the AKS application routing add-on, the OCI native
   ingress controller); section 1 (Cluster & ingress) of
   `docs/cloud/<your cloud>.md`.
5. Edit `deploy/overlays/byo/kustomization.yaml`'s `images:` entry to point
   at the image and digest from step 1 (`newName` and `digest`), replacing
   the base's placeholder `sha256:REPLACE_WITH_THE_RELEASE_DIGEST`, which
   `make preflight` refuses.
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
session, except on a site an admin has approved for the network (below).
There is no Referer-based attribution anywhere in this package. Signing in on the base host does not, by itself,
authenticate you on `alice.<base>`: the first time you open a page on an
owner host, the server sets a short-lived nonce cookie on that host,
redirects you to `<base>/auth/handoff` to mint a one-time code bound to
that nonce, and redirects you back to redeem it and mint that host's own
`__Host-sh_session` cookie. This is transparent in a browser (it is one
extra redirect hop on first visit to each host) and is exactly what
`scripts/smoke.sh`'s `hand_off_session` helper drives end to end with curl
and a cookie jar, if you want to see the wire protocol.

Every site has an access level, set from `/dashboard`'s "Your sites" panel
("Who can open it") or with `POST /api/sites/{site}/access`
(`POST /api/collaboration/sites/{owner}/{sitename}/access` for a team's
site), body `{"level": "..."}`:

| Level | Who can open it |
|---|---|
| `only_me` | The owner, or the team's members for a team site. The default for a new site. |
| `specific` | Also the named viewers (people or teams). |
| `company` | Anyone signed in with the link. |
| `listed` | Company, and shown in the showcase and search. |
| `network` | Anyone who can reach the server, with no sign-in. |

`network` is a request: it needs a `reason`, returns `202`, and the site
keeps its level until an admin approves it under "Access requests" on
`/admin` (or `POST /api/admin/access-requests/{owner}/{sitename}/approve`,
`/decline`, `/revoke`). Nobody approves their own request; set
`NETWORK_ACCESS_APPROVALS=2` to require two different admins. Anonymous visitors to a network site can read its
pages, assets and saved data and change nothing; a signed-in person adds
`?signin` to the address to use their own rights. The owner can move a site
to any lower level at any time; that ends its network approval.

Named viewers are managed under a site's "Viewers" section or through the
API (`GET`/`POST /api/collaboration/sites/{owner}/{sitename}/viewers`,
`DELETE .../viewers/{username}`). Adding one sets the site to `specific`;
removing the last one leaves it there, open only to the owner or team. A
`specific` site lives on its own dedicated hostname,
`<owner>--<site>.simple-host.127-0-0-1.nip.io` on the local overlay, and
stops being reachable at the ordinary `alice.<base>/<site>/` address at
all — anyone it is not shared with gets `404`, not `403`, so the site's
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

A page's own JavaScript calls
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

Anyone who can open a site can read and write its saved data as
themselves. The last 20 versions are kept; the owner or a team member can
list them and restore one with
`GET /api/collaboration/sites/{owner}/{sitename}/state-versions[/{id}]` and
`POST .../state-versions/{id}/restore` (a restore is a new write, audited
as `state_restore`).

Every state and asset write is attributed to the viewer and the site it
was made through (`via_site`); this is exactly what the `state_write`,
`asset_create`, and `asset_delete` audit rows in section 8 below record,
including the distinction between the claimed site and the one the browser
actually corroborated (`via_site_observed`).

## 8. The audit trail and access log

Commands in sections 8 and 9 name the cluster with `--context "$CTX"`: set
`CTX` to your kubectl context first (`docker-desktop` for the local
cluster).

Every mutation writes an `audit_events` row.
These actions write it inside the same database transaction as the
change it records (`internal/audit`'s `RecordTx`), so a mutation in this
group without its audit row cannot commit: `site_create`, `site_update`,
`site_delete`, `site_rollback`, `site_access`, `network_access_requested`,
`network_access_approval_added`, `network_access_approved`,
`network_access_declined`,
`network_access_reverted`, `state_restore`, `viewer_grant`,
`viewer_revoke`, `team_create`,
`team_delete`, `member_add`, `member_remove`, `state_write`,
`asset_create`, and `asset_delete` — the last three from the site-facing
API (`internal/handler/site_api.go`'s `PutState`/`PutStateVersioned`/
`CreateAsset`/`DeleteAsset`) and, for `asset_delete`, also from the
dashboard's base-host mirror (`internal/handler/assets_admin.go`'s
`deleteCollaborationAsset`) — both call sites now share this guarantee.
For both asset-delete call sites, the bucket object is not deleted inside
that transaction: the transaction queues it for the sweep, which deletes it
an hour later (`docs/storage.md`, Retention), so the database row, its
audit row and the queued deletion commit or roll back together. Ten actions remain best-effort,
written through the older `Record` with no shared transaction: `sign_in`,
`sign_out`, `session_revoke`, `key_mint`, `key_revoke`, `hand_off`, and the
admin actions `admin_disable_user`, `admin_enable_user`,
`admin_archive_versions`, `admin_export`. See
`docs/security-review.md`'s S9a row for the same inventory.
`state_write` coalesces repeated writes from the same actor
and site into one row per five-minute window (`detail.count`), so autosave
at page frequency does not bury every other action; every other action is
one row each. Separately, `access_log` records every view of hosted
content, including the owner's and team members' own — the `isSelfTraffic`
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
caller (`400` without one). Under the default `ACCESS_LOG_VISIBILITY=counts`
(`docs/configuration.md`) a non-admin gets aggregates only —
`{from, to, unique_viewers, days: [{day, views, unique_viewers}]}`, the
last 30 days unless `from`/`to` say otherwise — never who. `owner` returns
rows with user ids but redacts `ip`/`user_agent`; `admin` closes
`/api/access` to non-admins entirely. Admins always see full rows.

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
It also deletes sessions that ended (expired or signed out) more than
`ACCESS_LOG_RETENTION_DAYS` ago, with the IP and user agent they recorded,
and hand-off codes older than a day.
It runs under the database's owning credential, not `simplehost_app`: the
application role has no `DROP` privilege on either table at all, so retention can only ever run as the owning role. Preview what a
run would drop without dropping anything:

```sh
kubectl --context "$CTX" -n simple-host create job simple-host-prune-preview \
  --from=cronjob/simple-host-prune --dry-run=client -o json \
  | python3 -c "import json,sys; j=json.load(sys.stdin); j['spec']['template']['spec']['containers'][0]['args']=['prune','-dry-run']; print(json.dumps(j))" \
  | kubectl --context "$CTX" -n simple-host create -f -
kubectl --context "$CTX" -n simple-host wait --for=condition=complete job/simple-host-prune-preview --timeout=60s
kubectl --context "$CTX" -n simple-host logs job/simple-host-prune-preview
kubectl --context "$CTX" -n simple-host delete job simple-host-prune-preview
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
the caller to share it), unlike every other action above. There is no site-transfer feature and
no per-site write-mode setting, so neither has an audit action.

## 9. Backup and restore

The bucket is the site store, not a copy of it: every version and asset is
written there with a server-side-encryption header (`BACKUP_SSE`, default
`AES256`); set `BACKUP_ENVELOPE_KEY` for an additional client-side envelope
that makes the objects unreadable to anyone who can read the bucket but not
your Kubernetes Secrets. Escrow that key before first use: losing it loses
every site. Recovery comes from bucket versioning, which must be
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
Postgres, along with accounts, keys and the audit trail. Nothing in this
package backs up Postgres. A real install requires managed Postgres with
point-in-time recovery (7 days' retention or more recommended); see section
3 (Postgres) of `docs/cloud/<your cloud>.md` for the per-cloud setting.
`deploy/components/postgres-incluster` is for evaluation only: `make
preflight` refuses it in any overlay other than `deploy/overlays/local`
(override with `ALLOW_INCLUSTER_POSTGRES=1`), and the server logs a warning
at every start while it runs against it.

## 10. Upgrade

A deploy is: pick the release (`CHANGELOG.md` lists them), resolve and
verify its digest, update the overlay's `images:` entry (or your CI's
equivalent), `kubectl apply` the rendered overlay, and watch the rollout.
`kubectl -n simple-host exec deploy/simple-host -- /simple-host version`
prints the running release, commit and schema
(`simple-host v1.1.3 (commit <sha>, schema 0034)`); the server logs the
same line at startup.

**From v1.0.x**, sites move from the volume to the bucket: follow
`docs/storage.md`, "Migrating from a PVC install", not a plain apply. Its
step 1 backs up the database, step 4 is a dry run that changes nothing,
and step 5 is the one that applies the migrations.

The migrate init container applies each migration file and its
`schema_migrations` row in one transaction, so a failed file leaves nothing
half-applied. It takes a Postgres advisory lock and waits up to `-lock-wait`
(default 5m) for another migrator; each statement runs with
`lock_timeout=15s` and `statement_timeout=10min`, so a migration that
cannot get its locks fails and the init container retries, rather than
blocking live traffic. `/simple-host migrate -status` shows what is pending.

**Rollback.** `kubectl -n simple-host rollout undo deploy/simple-host`, or set
the previous digest in the overlay and re-apply. A migration file that
begins with `-- simple-host: backward-compatible` is additive only (new
tables, columns or indexes older code ignores). From v1.1.0 on, a server
starts against a schema newer than it knows only if every newer migration
carries that marker; otherwise it refuses with `schema version N is newer
than this binary knows`. v1.0.0 has the strict check, so rolling back to
v1.0.0 after any upgrade that added a migration is not possible. When a
rollback is refused, either roll forward (fix and ship a new image), or
restore the database with your managed PITR to a time before the upgrade,
which loses every write since then.

### Rate limits and replicas

Every limit is keyed per caller (`docs/security-review.md`, S15). The
security-relevant ones are counted in Postgres (table
`rate_limit_counters`, migration 0039), so they hold across any number of
replicas; the rest are counted in each pod's memory, so N replicas allow
N times their budget.

| Limit | Routes | Budget | Counted |
|---|---|---|---|
| `auth-client` | `/auth/login`, `/auth/callback`, `/auth/handoff`, the host gate's `/auth/session`, `GET /oauth/authorize` | 20 per 100 s per address (per user once signed in) | Shared |
| `api-key-mint` | `POST /api/keys` | 30 per 300 s per user | Shared |
| `oauth-register` | `POST /oauth/register` | 30 per 300 s per address | Shared |
| `oauth-token` | `POST /oauth/token`, `POST /oauth/revoke` | 120 per 60 s per address | Shared |
| state, management, admin, search | the site-facing state API, management and admin routes, search | as in `internal/handler/abuse_limits.go` | Per pod |

Shared limits are the ones a guessing or minting attacker would spread
over replicas: sign-in, session hand-off, API key mint, and the
connector's token and registration endpoints. Each is a fixed window
derived from the in-memory token bucket it replaces (limit = burst,
window = burst / refill rate: the same long-run rate and burst), one
database statement per request, with `Retry-After` set to the end of the
window. The window is computed by the database, so pods with skewed clocks
agree. Keys are SHA-256 digests; no address, user id or email is stored.
Rows are deleted by the server itself, at most once a minute per pod, an
hour after their window started — nothing to schedule.

The per-pod limits are abuse and cost guards (state reads and writes,
management calls, search) where a larger budget with more replicas is
acceptable and a database round trip on every request is not worth it.
If the database cannot answer a shared check within a second, that
request is counted against the pod's in-memory limit instead (logged at
most once a minute): sign-in is neither locked out nor left unlimited
during a database blip. During a rolling upgrade from v1.1.x, old pods
still count in memory until they are replaced.

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

- **The pod exits at start and crash-loops with `discover OIDC provider`.**
  The server fetches the issuer's discovery document at startup and exits
  if it cannot; it does not start and retry. Check the issuer from inside
  the cluster (`INSTALL.md` section 1), then fix the egress path or
  `OIDC_ISSUER`.

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
  never set either on a real install. The same holds for the OIDC issuer and
  the endpoints its discovery document names: `https://` only, unless
  `OIDC_INSECURE_ALLOWED=true` (local evaluation only).
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
  `/auth/*` is rate-limited by client address (20 requests per 100-second
  window, shared by every replica), and each sign-in-driven check in the
  smoke and pen-test suites uses several requests against it. Wait two to
  three minutes between runs, or space out individual `go test -run`
  invocations.
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

## 12. Monitoring

The server exposes Prometheus metrics at `GET /metrics` on its own
listener, port 9090 (`METRICS_PORT`; container port `metrics`). The Service
and the Ingress do not expose it: scrape the pods directly, with a
PodMonitor, pod annotations, or any in-cluster scraper.

| Metric | What it is |
|---|---|
| `simplehost_http_requests_total{code}` | Requests by class: `2xx`, `3xx`, `4xx`, `5xx` |
| `simplehost_http_request_duration_seconds` | Request latency histogram |
| `simplehost_db_connections_open`, `simplehost_db_connections_in_use` | Database pool |
| `simplehost_db_wait_count_total`, `simplehost_db_wait_seconds_total` | Waits for a free connection |
| `simplehost_build_info{version,commit,schema}` | The running release |
| `simplehost_bucket_ok` | 1 when the last bucket check (made by `/readyz`) succeeded, 0 when it failed |

Probes: `/healthz` is liveness and checks nothing else. `/readyz` checks
that the database is reachable and the schema is current; its result is
cached for 10 seconds, and a failure is logged as
`readyz: not ready: database: ...` (or `schema: ...`). It also checks that
the bucket can still be read with the configured credentials, but a bucket
fault leaves the pods Ready: every pod shares the bucket, so failing
readiness would take them all out and stop even the cached pages. The
failure is logged as `readyz: bucket: ...` and sets `simplehost_bucket_ok`
to 0.

No alerting stack ships with the package. What to watch:

- Pods not Ready, restarts, `CrashLoopBackOff`.
- `simplehost_bucket_ok` at 0, or `readyz: bucket:` in the logs. Pods stay
  Ready and cached pages keep serving, but uncached pages answer 503 and
  publishing fails until the bucket is fixed.
- Failed CronJob runs (`kube_job_status_failed`), the prune job included.
- The 5xx rate from `simplehost_http_requests_total`.
- Certificate expiry (`certmanager_certificate_expiration_timestamp_seconds`,
  or your cloud's managed certificate).
- `simplehost_db_connections_in_use` near the pool limit.
- The managed database's storage and PITR status.
