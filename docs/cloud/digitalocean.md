# DigitalOcean (DOKS)

Verified 2026-10-01 on DigitalOcean (nyc3): DOKS 1.35 (two `s-2vcpu-4gb` nodes), the
1-Click's `deploy.sh` (1 m 46 s to a load balancer IP), Managed PostgreSQL 16 over its
VPC host with `verify-full` as `doadmin`, a Spaces bucket with a bucket-limited key, and
certificates for the base host and each person's wildcard from Let's Encrypt (staging)
by DNS-01 in DigitalOcean DNS. The chart was 0.1.0 with an image built from `main`
(`BACKUP_SSE=none` is newer than v1.9.1). `make smoke` passed 101 of 102; the one
difference is Traefik collapsing `//` in a path, which the smoke test expects kept.
Visitors' own addresses reached the server through the load balancer's PROXY protocol,
every object in the bucket carried the envelope, and `upgrade.sh` and `uninstall.sh`
worked.

Fills the sections in [README.md](README.md). On DigitalOcean a Helm chart replaces the
kustomize overlay: values go in one values file, not `config.env` / `secrets.env`.

The Kubernetes 1-Click installs Traefik (its own IngressClass, `simple-host`, in
`simple-host-ingress`, so a controller the cluster already runs is left alone),
cert-manager if the cluster has none, and the chart with no settings: the database
starts, and Simple Host waits for the values below. Chart 0.1.1 still has those
DigitalOcean defaults; 0.2.0 is provider-neutral and needs the preset
(`deploy/helm/simple-host-enterprise/values-digitalocean.yaml`) for Spaces, the
Traefik class, NetworkPolicy, and a Let's Encrypt DNS-01 issuer on DigitalOcean
DNS. Marketplace workflows can pin `--version 0.1.1`. Without the 1-Click, install
cert-manager and a controller, then:

```sh
helm install simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.2.0 -n simple-host --create-namespace -f deploy/helm/simple-host-enterprise/values-digitalocean.yaml -f my-values.yaml
```

The values sections 2 to 6 below give you, in `my-values.yaml` (keep it out of git: it
holds secrets). Pass the preset first so Spaces, the Traefik class and the
DigitalOcean issuer stay set:

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
  sse: none
certificates:
  createIssuer: true
  issuer: simple-host-letsencrypt
  acme:
    email: you@example.com
    digitaloceanToken: <token with Domains read and write>
ingress:
  className: simple-host
networkPolicy:
  ingressNamespace: simple-host-ingress
```

After the 1-Click, apply them with:

```sh
helm upgrade simple-host-enterprise oci://ghcr.io/vineetu/charts/simple-host-enterprise --version 0.2.0 -n simple-host --reset-then-reuse-values -f deploy/helm/simple-host-enterprise/values-digitalocean.yaml -f my-values.yaml
```

The chart generates the session key, the application-role password and the envelope
key on first install and keeps them across upgrades. External `postgres.password` is
the existing owning role's password. Every value, with its default, is in the
chart's `values.yaml`. Leftover application settings the chart does not already own
go under `extraConfig` (secrets under `extraSecrets`; certificate lifecycle is
`certificates.*`). The chart's `README.md` covers upgrading and removing.

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

- The certificates need DNS-01. With the DigitalOcean preset (`createIssuer: true`)
  the chart creates a Let's Encrypt ClusterIssuer (`simple-host-letsencrypt`) that
  answers DNS-01 in DigitalOcean DNS, limited to names under `<host>`. Chart 0.2.0
  without the preset uses a ClusterIssuer already in the cluster instead. The
  domain's DNS has to be hosted at DigitalOcean (Networking, Domains): if it is
  registered elsewhere, set its nameservers at the registrar to
  `ns1.digitalocean.com`, `ns2.digitalocean.com` and `ns3.digitalocean.com`.
- The token needs read and write on Domains. A DigitalOcean token reaches every domain
  in its team, so keep Simple Host's domain in a team of its own. Pass the token as a
  Secret you create in cert-manager's namespace with
  `certificates.acme.digitaloceanTokenSecret`, rather than as a value, to keep it out
  of Helm's release history. The file holds the token with no trailing newline:

  ```sh
  kubectl -n cert-manager create secret generic simple-host-do-dns --from-file=access-token=do-token.txt
  ```
- **Owner certificates.** Each person with sites gets `*.<name>.<host>` from the same
  issuer; the run above had each one Ready about two minutes after the person's first
  site. While a DNS-01 challenge is open, its TXT record hides the `*.<host>` record for
  that person's names, and a resolver can cache the empty answer for up to the zone's
  negative TTL (30 minutes at DigitalOcean). Simple Host only links to those names once
  the certificate is Ready. Let's Encrypt allows 50 new certificates per registered domain per week, so an
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
  days here). Spaces stores the rule as given, but then reports an expiry date on
  current objects too (`x-amz-expiration`), a known fault in how Ceph, which Spaces runs
  on, computes that header; the stored rule only touches noncurrent versions:

  ```sh
  aws s3api put-bucket-lifecycle-configuration --endpoint-url https://nyc3.digitaloceanspaces.com --bucket <bucket> --lifecycle-configuration '{"Rules":[{"ID":"simple-host","Status":"Enabled","Filter":{},"NoncurrentVersionExpiration":{"NoncurrentDays":30},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":7},"Expiration":{"ExpiredObjectDeleteMarker":true}}]}'
  ```

- Spaces accepts the SSE-S3 header but does not apply it (the stored object reports no
  server-side encryption), so the chart sets `BACKUP_SSE=none` and turns the envelope on
  instead: every object is encrypted in the pod before it is sent. The
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
