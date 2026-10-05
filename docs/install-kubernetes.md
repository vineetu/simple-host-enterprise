# Install on an existing Kubernetes cluster

Simple Host Enterprise installs into a Kubernetes cluster you already run.
It does not create a cluster, a VPC, or other cloud account resources. The
tunable configuration is the Helm chart's values
(`deploy/helm/simple-host-enterprise/values.yaml`). Postgres is an existing
database, or an optional chart-managed instance for evaluation. Object
storage is an existing S3-compatible bucket. Sign-in is the company's OIDC.
Ingress and TLS use the cluster's controller; cert-manager is required for
automatic owner certificates, a chart-created issuer, and in-cluster Postgres.

The kustomize overlays (`INSTALL.md`, `docs/install.md`) remain. Cloud
Terraform that provisions a cluster is an advanced path (`deploy/terraform/`,
`docs/cloud/`). DigitalOcean-specific chart settings are
`values-digitalocean.yaml` (`docs/cloud/digitalocean.md`); marketplace
1-Clicks that still want chart 0.1.x defaults can pin `--version 0.1.1`.

## Prerequisites

- Kubernetes 1.30 or later, and Helm 3.14 or later (`--reset-then-reuse-values`).
- An ingress controller. Empty `ingress.className` uses the cluster's default
  IngressClass; set a name when the cluster has several, or none is the default.
  Uploads are 100 MiB by default: keep the controller's body limit above that
  (`ingress.annotations` already sets ingress-nginx's).
- cert-manager when `certificates.ownerCerts` is `auto` (the default), when
  `certificates.createIssuer` is true, or when `postgres.mode` is `incluster`
  (the evaluation database's TLS comes from a private CA cert-manager issues).
  With `ownerCerts: manual` and an existing `ingress.tlsSecret`, the application
  Ingress does not request a replacement certificate.
- Postgres 16 reachable from the cluster, TLS with `verify-full` and the
  provider's CA, storage encryption at rest, point-in-time recovery. The
  owning role must be able to create tables and roles (migration 0020 creates
  `simplehost_app`). Nothing in this package backs up Postgres.
- An S3-compatible bucket over `https://`, with versioning and a lifecycle
  rule (`docs/storage.md`). A local evaluation cluster may use `http://` only
  with `extraConfig.BACKUP_STORAGE_INSECURE_ALLOWED: "true"`.
- The company's OIDC provider, reachable from the pods.

The image is `ghcr.io/vineetu/simple-host-enterprise:v0.9.3`, pinned by digest
in the chart. Pin the chart version; 0.2.1 is this release.

## Review the configured chart download

The [Enterprise setup helper](https://simple-host.app/setup?product=enterprise)
downloads a normal Helm chart directory as one ZIP. Its `Chart.yaml` depends on
this published chart under alias `enterprise`; `values.yaml` contains the selected
configuration under `enterprise:`. The published dependency archive is bundled
under `charts/`, so reviewers can inspect its templates and defaults before
installing and do not need to fetch a dependency just to use the download.

Read the included README for checksum verification, template inspection,
`helm lint`, `helm template`, install dry-run, `helm install` and `helm upgrade`
against the local directory. Client dry-run does not apply resources; select
your intended Kubernetes context. `install.sh` is an optional convenience.
The existing Secret is supplied separately; `.helmignore` excludes filled
`secrets.env`, the CA file and review/render output from chart files. External
Postgres passes its CA as `--set-file enterprise.postgres.external.caCert=db-ca.crt`.

The direct-chart examples below use the upstream values layout. With the setup
helper's wrapper, put those keys under `enterprise:` and use the downloaded
README's local-chart commands instead.

## Values

Keep the values file out of git: it can hold secrets. The setup helper at
https://simple-host.app/setup (Enterprise) writes these keys under the wrapper's `enterprise:` alias.

```yaml
host: corp-sites.com
oidc:
  issuer: https://login.example.com
  clientId: <client id>
  clientSecret: <client secret>
  adminEmails: you@example.com
  allowedEmailDomains: example.com   # required with Google
postgres:
  mode: external
  user: simplehost
  password: <existing owning-role password>
  external:
    host: postgres.example.com
    port: 5432
    caCert: |
      -----BEGIN CERTIFICATE-----
      ...
storage:
  endpoint: https://s3.example.com
  region: eu-west-1
  bucket: <bucket>
  accessKeyId: <key>          # omit both keys when an existing AWS-compatible identity supplies them
  secretAccessKey: <secret>
  sse: AES256                 # aws:kms plus storage.sseKeyId; none with the envelope
certificates:
  issuer: company-ca          # required with ownerCerts=auto; omit with ownerCerts=manual and an existing tlsSecret
ingress:
  className: nginx            # omit to use the cluster default
```

`extraConfig` is leftover application settings from `docs/configuration.md`
that the chart does not already own as typed values (for example
`MAX_ARCHIVE_BYTES`, `OIDC_EMAIL_CLAIM`, and the evaluation override
`BACKUP_STORAGE_INSECURE_ALLOWED`). Secrets go in `extraSecrets`, or in
`secrets.existingSecret` when that is set. Certificate lifecycle is
`certificates.*` (`ownerCerts`, `issuer`, `createIssuer`); it is not extraConfig.

```yaml
extraConfig:
  MAX_ARCHIVE_BYTES: "209715200"
  OIDC_EMAIL_CLAIM: preferred_username
extraSecrets:
  SMTP_URL: smtp://user:password@mail.example.com:587
```

`extraSecrets` is only applied to the Secret the chart manages. With
`secrets.existingSecret`, put those keys in that Secret yourself; the chart
refuses both at once.

## Install

```sh
kubectl create namespace simple-host --dry-run=client -o yaml | kubectl apply -f -
kubectl label ns simple-host pod-security.kubernetes.io/enforce=restricted --overwrite
```

```sh
helm install simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.2.1 -n simple-host --create-namespace -f my-values.yaml
```

From this repository instead of the published chart:

```sh
helm install simple-host-enterprise deploy/helm/simple-host-enterprise -n simple-host --create-namespace -f my-values.yaml
```

Until `host`, `oidc.issuer`, `oidc.clientId` and `storage.bucket` are set, the
chart installs only the generated secrets and, with `postgres.mode: incluster`,
the evaluation database. Apply the rest with:

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.2.1 -n simple-host --reset-then-reuse-values -f my-values.yaml
```

Watch the migrate init container, then the rollout:

```sh
kubectl -n simple-host logs -l app=simple-host -c migrate --tail=50
kubectl -n simple-host rollout status deploy/simple-host --timeout=300s
curl -fsS https://<host>/readyz
```

Register `https://<host>/auth/callback` as the OIDC redirect URI (web
application, authorization code, `openid email profile`). Point `<host>` and
`*.<host>` at the ingress load balancer. `INSTALL.md` section 6 has the
provider notes (Okta, Entra ID, Google Workspace) and the DNS records.

The chart generates the session signing key, the application-role database
password and the envelope key on first install and keeps them across upgrades.
With `postgres.mode: external`, `postgres.password` is the existing owning
role's password; it is never generated. Back the envelope key up (the notes
print the command): every site is unreadable without it. Every value, secrets
included, is in Helm's release history; `secrets.existingSecret` and
`certificates.acme.digitaloceanTokenSecret` keep those credentials out of it.

## Existing Postgres or the chart's database

`postgres.mode: external` is the real install: an existing Postgres 16, port
5432 unless the provider says otherwise, `sslmode: verify-full`, CA in
`postgres.external.caCert`. The owning role (`postgres.user` /
`postgres.password`) runs migrations; the server connects as `simplehost_app`
with a distinct password the chart generates. `postgres.password` is that
existing owning role's password, never a generated value.

`postgres.mode: incluster` (the chart default) starts one Postgres in the
namespace, with TLS from a private CA cert-manager issues, so the server
still uses `verify-full`. Nothing backs it up. The server logs that at every
start (`DB_INCLUSTER_EVALUATION`). Choose before anyone publishes: switching
`postgres.mode` later starts from an empty database, and nothing is copied.

## Existing S3-compatible storage

Set `storage.endpoint` (https), `storage.region`, `storage.bucket`. Turn
versioning on, and a lifecycle rule that expires noncurrent versions after
your retention window. `storage.sse` is `AES256` by default; `aws:kms` needs
`storage.sseKeyId`; `none` is for stores without SSE-S3 and requires the
envelope key.

A local evaluation cluster may set `storage.endpoint` to an `http://` URL
only with the documented override, which does not change the production
https default:

```yaml
storage:
  endpoint: http://minio.minio.svc:9000
extraConfig:
  BACKUP_STORAGE_INSECURE_ALLOWED: "true"
```

Leave `accessKeyId` / `secretAccessKey` empty when an existing
platform-managed identity on the `simple-host` ServiceAccount can reach the
bucket. Set `serviceAccount.annotations` (and `podAnnotations` / `podLabels`
if the platform needs them on the app pod). The chart does not create IAM
roles or cloud identity bindings.

The default AWS SDK credential chain supports AWS-compatible mechanisms
(IRSA, EKS Pod Identity, static keys in the environment). It does not speak
Azure Workload Identity or GCS Workload Identity. Other S3-compatible
providers use their static S3/HMAC keys. The owner-hosts controller has no
storage access and does not receive these annotations. The ServiceAccount
keeps `automountServiceAccountToken: false`; the platform injects its own
credential when it needs to.

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::<account>:role/<role>
```

## Existing issuer and ingress

`certificates.createIssuer` is false. With `certificates.ownerCerts: auto`
(the default), set `certificates.issuer` to a ClusterIssuer already in the
cluster. The base Ingress is annotated with it, and the owner-hosts
reconciler uses the same name (`OWNER_CERT_ISSUER`). The issuer must be able
to sign `<host>` and `*.<host>` (DNS-01 for an ACME wildcard, or an internal
CA). Each person also needs `*.<name>.<host>` from the same issuer
(`INSTALL.md`, "Site addresses").

`certificates.ownerCerts: manual` leaves the reconciler out. `issuer` may be
empty: the chart omits `cert-manager.io/cluster-issuer` so an existing
`ingress.tlsSecret` is used without cert-manager requesting a replacement.
You still issue each `*.<name>.<host>` yourself, and that certificate and
Ingress host rule must exist before that owner publishes a first site. For
owner `alice` (copy class and controller annotations from the base Ingress):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: sh-owner-alice
  namespace: simple-host
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: 128m
spec:
  # ingressClassName: the same as the simple-host Ingress, if set there
  tls:
    - hosts: ["*.alice.<host>"]
      secretName: sh-owner-alice-tls
  rules:
    - host: "*.alice.<host>"
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

Any other way of getting a `*.alice.<host>` certificate onto that Ingress
works too. `INSTALL.md` ("Issuing owner certificates yourself") has the
cert-manager Certificate form of the same obligation.

`certificates.createIssuer: true` is the DigitalOcean opt-in: a Let's Encrypt
ClusterIssuer that solves DNS-01 in DigitalOcean DNS, limited to names under
`<host>`. It needs `certificates.acme.email` and a Domains token
(`digitaloceanToken` or `digitaloceanTokenSecret`). The preset
`values-digitalocean.yaml` turns this on.

Empty `ingress.className` omits `ingressClassName` and uses the cluster
default. Set `networkPolicy.ingressNamespace` to the controller's namespace
to admit only that namespace to the server; empty leaves the server open to
the cluster.

## OIDC

Required: `oidc.issuer`, `oidc.clientId`, and `oidc.clientSecret` (or
`OIDC_CLIENT_SECRET` in `secrets.existingSecret`). Redirect URI, exactly:
`https://<host>/auth/callback`. With `https://accounts.google.com`,
`oidc.allowedEmailDomains` is required. Optional claims, scopes and admin
group mappings that are not first-class values go in `extraConfig`
(`OIDC_EMAIL_CLAIM`, `OIDC_ADMIN_CLAIM`, `OIDC_ADMIN_VALUE`, …).

## Restore and backups

Sites live in the bucket. Versioning and the lifecycle rule are how you
recover a site; the envelope key is required to read it. Nothing in this
package backs up Postgres. A managed database's point-in-time recovery is
the database backup. `postgres.mode: incluster` has none
(`DB_INCLUSTER_EVALUATION`); do not treat it as recoverable.

## Upgrade and remove

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version <new> -n simple-host --reset-then-reuse-values
```

`--reset-then-reuse-values` keeps your settings and takes the new chart's
defaults, including a new image digest. `--reuse-values` would keep the old
image.

```sh
helm uninstall simple-host-enterprise -n simple-host
```

Uninstall keeps `simple-host-secrets` (the envelope key) and the in-cluster
database volume, so a reinstall under the same release name picks both up.
It never touches the bucket or a managed database. Deleting the namespace
removes the Secret and the volume. Export and DNS order: `docs/uninstall.md`.

## Render the same chart as YAML

`helm template` and Argo CD do not read a live Secret back, so generated keys
would change on every render. Create the namespace and a persistent Secret
first, then point the chart at it.

The Secret must hold the keys `deploy/overlays/byo/secrets.env.example`
lists. Generate session and envelope keys once, keep the file in the
organisation's secret store, and never commit it. With `postgres.mode:
external`, `DB_PASSWORD` is the existing owning role's password; do not
generate it. `DB_APP_PASSWORD` may be generated (migrate creates
`simplehost_app`).

```sh
kubectl create namespace simple-host --dry-run=client -o yaml | kubectl apply -f -
kubectl label ns simple-host pod-security.kubernetes.io/enforce=restricted --overwrite
```

```sh
f=/tmp/simple-host-secrets.env
(umask 077; cat > "$f" <<EOF
SESSION_SIGNING_KEY=k1:$(openssl rand -base64 32)
DB_PASSWORD=
DB_APP_PASSWORD=$(openssl rand -hex 24)
BACKUP_ENVELOPE_KEY=k1:$(openssl rand -base64 32)
OIDC_CLIENT_SECRET=
BACKUP_STORAGE_ACCESS_KEY_ID=
BACKUP_STORAGE_SECRET_ACCESS_KEY=
EOF
)
```

Fill `DB_PASSWORD` with the existing owning role password, then
`OIDC_CLIENT_SECRET` (and the bucket keys, if you are not using an
AWS-compatible workload identity) with a hidden prompt. Change `k=` for
each empty key. `umask 077` applies to the temp rewrite so the file stays
mode 600 throughout:

```sh
(umask 077; k=DB_PASSWORD; f=/tmp/simple-host-secrets.env; printf '%s: ' "$k"; read -rs v && K="$k" V="$v" awk 'BEGIN{FS=OFS="="} $1==ENVIRON["K"]{$0=ENVIRON["K"] "=" ENVIRON["V"]} {print}' "$f" > "$f.new" && mv "$f.new" "$f" && echo " saved")
```

```sh
kubectl -n simple-host create secret generic simple-host-secrets --from-env-file=/tmp/simple-host-secrets.env --dry-run=client -o yaml | kubectl apply -f -
```

`DB_PASSWORD` and `DB_APP_PASSWORD` must differ. Escrow `BACKUP_ENVELOPE_KEY`
before the first deploy. Then in values:

```yaml
secrets:
  existingSecret: simple-host-secrets
oidc:
  clientSecret: ""
storage:
  accessKeyId: ""
  secretAccessKey: ""
  envelopeKey: ""
postgres:
  password: ""
  appPassword: ""
```

Render, then apply:

```sh
helm template simple-host-enterprise deploy/helm/simple-host-enterprise -n simple-host -f my-values.yaml --set secrets.existingSecret=simple-host-secrets > simple-host.yaml
kubectl apply -n simple-host -f simple-host.yaml
```

The chart still emits ConfigMaps, Deployments, Ingress and the rest; it does
not emit `simple-host-secrets`. Changing a value in the existing Secret needs
a rollout restart:

```sh
kubectl -n simple-host rollout restart deploy/simple-host
```

Resources, `nodeSelector`, `tolerations`, `affinity` and `image.pullSecrets`
are chart values and land on the pods. `replicas`, `prune.schedule`,
`trustedProxyCIDRs` and `networkPolicy` are the rest of the install shape.

Your person address can open a selected home site; its showcase has a signed-in
feed, bio, pins and manual order. Access and site origins stay in force. See
[Your home page](your-home-page.md).
