# DigitalOcean (DOKS)

Not yet run on a DigitalOcean account. The Helm chart this page uses
(`deploy/helm/simple-host-enterprise`) passed `make smoke` 105 of 105 on a local
two-node kind cluster, 2026-09-30, with Spaces-style storage (`BACKUP_SSE=none` and the
envelope key).

Fills the sections in [README.md](README.md). On DigitalOcean the chart replaces the
kustomize overlay: values go in a values file, not `config.env` / `secrets.env`.

## Two ways in

- **The Kubernetes 1-Click** (Marketplace, "Simple Host Enterprise"). It installs
  ingress-nginx and cert-manager if the cluster has none, then the chart with no
  settings: the database starts, and Simple Host waits for the values in "Finish setup"
  below.
- **Helm yourself**, on any DOKS cluster:

  ```sh
  helm install simple-host oci://ghcr.io/vineetu/charts/simple-host-enterprise -n simple-host --create-namespace -f my-values.yaml
  ```

  The chart needs cert-manager and an ingress controller already in the cluster. The
  1-Click's `deploy.sh` shows the versions it installs.

## Finish setup

Write `my-values.yaml` with what sections 2 to 6 below give you, then:

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise -n simple-host --reuse-values -f my-values.yaml
```

(With Helm yourself, the release is whatever you named it, `simple-host` above.)

```yaml
host: sites.example.com
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

Keep the file out of git: it holds secrets. The chart generates the session key, the
database passwords and the envelope key on first install and keeps them across
upgrades. Every value the chart takes, with its default, is in the chart's
`values.yaml`; any setting from `docs/configuration.md` goes under `extraConfig`.

## 1. Cluster & ingress

- A DOKS cluster on Kubernetes 1.31 or later. Two `s-2vcpu-4gb` nodes run two replicas
  on different nodes, with room for the in-cluster database.
- The 1-Click installs ingress-nginx behind a DigitalOcean load balancer with PROXY
  protocol on at both ends, so each visitor's own address reaches the server. Installing
  ingress-nginx yourself, set the same two things:
  `service.beta.kubernetes.io/do-loadbalancer-enable-proxy-protocol: "true"` on the
  controller's Service and `use-proxy-protocol: "true"` in its ConfigMap.
- The chart sets the body cap (128 MiB) and the 300 s timeouts as ingress-nginx
  annotations (`ingress.annotations`). For another controller, replace them with that
  controller's equivalent (`deploy/overlays/byo/ingress-patch.yaml` has the table).
- `trustedProxyCIDRs` can stay empty: the controller's pods have private addresses, and
  the built-in private ranges cover them.

## 2. DNS & wildcard certificate

- Two `A` records, `<host>` and `*.<host>`, to the load balancer's IP:

  ```sh
  kubectl -n ingress-nginx get svc ingress-nginx-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
  ```

- The certificates need DNS-01. By default the chart creates a Let's Encrypt
  ClusterIssuer (`simple-host-letsencrypt`) that answers DNS-01 in DigitalOcean DNS, so
  the domain's DNS has to be hosted at DigitalOcean (Networking, Domains). If the
  company's domain lives elsewhere, delegate just the Simple Host domain (an `NS` record
  at the parent for `<host>`) to `ns1.digitalocean.com`, `ns2…` and `ns3…`.
- The token (`certificates.acme.digitaloceanToken`) needs read and write on Domains and
  nothing else. The chart stores it in the `cert-manager` namespace, where a
  ClusterIssuer reads it.
- **Owner certificates.** Each person with sites gets `*.<name>.<host>` from the same
  issuer. Let's Encrypt allows 50 new certificates per registered domain per week, so an
  organisation that onboards more people than that in a week should use its own CA:
  set `certificates.createIssuer: false` and `certificates.issuer: <your ClusterIssuer>`.

## 3. Postgres

The 1-Click runs Postgres in the cluster (`postgres.mode: incluster`) so it can start
with nothing to configure. Nothing backs that one up, and the server says so in its
log at every start. For real use, DigitalOcean Managed PostgreSQL 16:

- Create it in the cluster's region and VPC. Under Settings, Trusted sources, add the
  Kubernetes cluster.
- Create a database named `simplehost` (Users & Databases).
- From Connection details, choose **VPC network**. Download the CA certificate.
- Values:

  ```yaml
  postgres:
    mode: external
    user: doadmin
    password: <doadmin password>
    external:
      host: private-<name>-do-user-<id>-0.<region>.db.ondigitalocean.com
      port: 25060
      caCert: |
        -----BEGIN CERTIFICATE-----
        ...
  ```

- `doadmin` is the owning role migrations run as; the first migration creates the
  separate `simplehost_app` role the server connects as.
- Daily backups with seven days of point-in-time recovery are on by default, and the
  storage is encrypted at rest.

## 4. Bucket

- A Spaces bucket in the cluster's region. `storage.endpoint` is
  `https://<region>.digitaloceanspaces.com` and `storage.region` is the region
  (`nyc3`, `ams3`, `sgp1`, …).
- Turn versioning on (versioning is the file backup). Spaces has no switch for it in
  the control panel; any S3 client can do it:

  ```sh
  aws s3api put-bucket-versioning --endpoint-url https://nyc3.digitaloceanspaces.com --bucket <bucket> --versioning-configuration Status=Enabled
  ```

- Add a lifecycle rule that expires noncurrent versions after your retention window
  (`aws s3api put-bucket-lifecycle-configuration`, same endpoint).
- Spaces supports no SSE-S3, so the chart sets `BACKUP_SSE=none` and turns the envelope
  on instead: every object is encrypted in the pod before it is sent. The server refuses
  `none` without an envelope key. **Back the key up** (the chart's notes print the
  command): every site is unreadable without it.

## 5. Identity for pods

DigitalOcean has no workload identity for Spaces. Create a Spaces access key limited to
the bucket (API, Spaces Keys, Limited access, Read/Write/Delete on that bucket) and put
it in `storage.accessKeyId` / `storage.secretAccessKey`.

## 6. OIDC provider notes

As on every cloud: see `docs/install.md` section 6. Register
`https://<host>/auth/callback` as the redirect URI. With Google, `allowedEmailDomains`
is required.

## 7. Verify

```sh
kubectl -n simple-host rollout status deploy/simple-host
```

```sh
curl -fsS https://<host>/readyz
```

```sh
make smoke BASE=https://<host> KEY_FILE=~/.simple-host-install-key
```

## Upgrade and remove

- The 1-Click's `upgrade.sh`, or the same `helm upgrade … --reuse-values` with a newer
  chart. The chart pins the image by digest; a new release ships as a new chart
  version.
- `helm uninstall` keeps `simple-host-secrets` (it holds the envelope key) and the
  database volume. Deleting the `simple-host` namespace removes both. The Spaces bucket
  and a managed database are never touched.
