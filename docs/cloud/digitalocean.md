# DigitalOcean (DOKS)

Tested: the chart (`deploy/helm/simple-host-enterprise` 0.1.0, with an image built from
`main` for `BACKUP_SSE=none`), installed by the 1-Click's `deploy.sh` on a local
two-node kind cluster, passed `make smoke` 104 of 105 through Traefik, 2026-09-30. The
one difference: Traefik collapses `//` in a path, which the smoke test expects kept. Not
yet run on a DigitalOcean account.

Fills the sections in [README.md](README.md). On DigitalOcean a Helm chart replaces the
kustomize overlay: values go in one values file, not `config.env` / `secrets.env`.

The Kubernetes 1-Click installs Traefik (its own IngressClass, `simple-host`, in
`simple-host-ingress`, so a controller the cluster already runs is left alone),
cert-manager if the cluster has none, and the chart with no settings: the database
starts, and Simple Host waits for the values below. Without the 1-Click, install
cert-manager and a controller, then:

```sh
helm install simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.1.0 -n simple-host --create-namespace -f my-values.yaml
```

The values sections 2 to 6 below give you, in `my-values.yaml` (keep it out of git: it
holds secrets):

```yaml
host: corp-sites.com
oidc:
  issuer: https://accounts.google.com
  clientId: <client id>
  clientSecret: <client secret>
  adminEmails: you@example.com
  allowedEmailDomains: example.com
storage:
  endpoint: https://nyc3.digitaloceanspaces.com
  region: nyc3
  bucket: <bucket>
  accessKeyId: <Spaces key>
  secretAccessKey: <Spaces secret>
certificates:
  acme:
    email: you@example.com
    digitaloceanToken: <token with Domains read and write>
```

After the 1-Click, apply them with:

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.1.0 -n simple-host --reset-then-reuse-values -f my-values.yaml
```

The chart generates the session key, the database passwords and the envelope key on
first install and keeps them across upgrades. Every value, with its default, is in the
chart's `values.yaml`. Any setting from `docs/configuration.md` goes under
`extraConfig` (secrets under `extraSecrets`). The chart's `README.md` covers upgrading
and removing.

## 1. Cluster & ingress

- A DOKS cluster on Kubernetes 1.30 or later. Two `s-2vcpu-4gb` nodes run two replicas
  on different nodes, with room for the in-cluster database.
- The 1-Click's Traefik sits behind a DigitalOcean load balancer with PROXY protocol at
  both ends, so each visitor's own address reaches the server, and has 300 s timeouts
  and no body cap. With another controller, set `ingress.className` to its class,
  `networkPolicy.ingressNamespace` to its namespace, and the body cap and timeouts
  (`deploy/overlays/byo/ingress-patch.yaml` has them per controller).
- `trustedProxyCIDRs` can stay empty: the controller's pods have private addresses, and
  the built-in private ranges cover them. The chart's NetworkPolicy lets only the
  controller's namespace reach the server.

## 2. DNS & wildcard certificate

- Two `A` records, `<host>` and `*.<host>`, to the load balancer's IP:

  ```sh
  kubectl -n simple-host-ingress get svc traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
  ```

- The certificates need DNS-01. The chart creates a Let's Encrypt ClusterIssuer
  (`simple-host-letsencrypt`) that answers DNS-01 in DigitalOcean DNS, limited to names
  under `<host>`. The domain's DNS has to be hosted at DigitalOcean (Networking,
  Domains): if it is registered elsewhere, set its nameservers at the registrar to
  `ns1.digitalocean.com`, `ns2.digitalocean.com` and `ns3.digitalocean.com`.
- The token needs read and write on Domains. A DigitalOcean token reaches every domain
  in its team, so keep Simple Host's domain in a team of its own. Pass the token as a
  Secret you create in cert-manager's namespace (key `access-token`) with
  `certificates.acme.digitaloceanTokenSecret`, rather than as a value, to keep it out
  of Helm's release history.
- **Owner certificates.** Each person with sites gets `*.<name>.<host>` from the same
  issuer. Let's Encrypt allows 50 new certificates per registered domain per week, so an
  organisation that onboards more people than that in a week uses its own CA:
  `certificates.createIssuer: false` and `certificates.issuer: <your ClusterIssuer>`.

## 3. Postgres

The 1-Click runs Postgres in the cluster (`postgres.mode: incluster`) so it starts with
nothing to configure. Nothing backs that one up, and the server says so in its log at
every start. Choose before anyone publishes: switching `postgres.mode` later starts
from an empty database, and nothing is copied. For real use, DigitalOcean Managed
PostgreSQL 16:

- A database cluster for Simple Host alone, in the Kubernetes cluster's region and
  VPC: `doadmin`, which runs the migrations, reaches every database on it.
- Settings, Trusted sources: add the Kubernetes cluster.
- Users & Databases: create a database named `simplehost`.
- Connection details, VPC network: note the host and port, and download the CA
  certificate.
- Values:

  ```yaml
  postgres:
    mode: external
    user: doadmin
    password: <doadmin password>
    external:
      host: <private host from Connection details>
      port: 25060
      caCert: |
        -----BEGIN CERTIFICATE-----
        ...
  ```

- The migrations create the separate `simplehost_app` role the server connects as.
- Daily backups with seven days of point-in-time recovery are on by default, and the
  storage is encrypted at rest.

## 4. Bucket

- A Spaces bucket in the cluster's region. `storage.endpoint` is
  `https://<region>.digitaloceanspaces.com` and `storage.region` is the region
  (`nyc3`, `ams3`, `sgp1`, …).
- Versioning on (versioning is the file backup). Spaces has no switch for it in the
  control panel; any S3 client sets it:

  ```sh
  aws s3api put-bucket-versioning --endpoint-url https://nyc3.digitaloceanspaces.com --bucket <bucket> --versioning-configuration Status=Enabled
  ```

- A lifecycle rule that expires noncurrent versions after your retention window (30
  days here):

  ```sh
  aws s3api put-bucket-lifecycle-configuration --endpoint-url https://nyc3.digitaloceanspaces.com --bucket <bucket> --lifecycle-configuration '{"Rules":[{"ID":"simple-host","Status":"Enabled","Filter":{},"NoncurrentVersionExpiration":{"NoncurrentDays":30},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":7},"Expiration":{"ExpiredObjectDeleteMarker":true}}]}'
  ```

- Spaces does not support SSE-S3, so the chart sets `BACKUP_SSE=none` and turns the
  envelope on instead: every object is encrypted in the pod before it is sent. The
  server refuses `none` without an envelope key. **Back the key up** (the chart's notes
  print the command): every site is unreadable without it.

## 5. Identity for pods

DigitalOcean has no workload identity for Spaces. Create a Spaces access key limited to
the bucket (API, Spaces Keys, Limited access, Read/Write/Delete on that bucket) and put
it in `storage.accessKeyId` / `storage.secretAccessKey`.

## 6. OIDC provider notes

Register the app as in `INSTALL.md` section 6, step A (Google Workspace, Okta, Entra
ID). Redirect URI: `https://<host>/auth/callback`. With Google, `allowedEmailDomains`
is required. DOKS nodes reach the internet by default, so a cloud identity provider is
reachable from the pods.

## 7. Verify

```sh
kubectl -n simple-host rollout status deploy/simple-host
```

```sh
curl -fsS https://<host>/readyz
```

The issuer from inside the cluster (in `default`: `simple-host` admits only restricted
pods):

```sh
kubectl run oidc-check -n default --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
```

`KEY_FILE` holds a Full-scope API key an admin makes on `/dashboard`:

```sh
make smoke BASE=https://<host> KEY_FILE=~/.simple-host-install-key
```
