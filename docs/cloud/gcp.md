# Google Cloud (GKE)

Not yet run end to end on this cloud.

Fills the sections in [README.md](README.md).

## 1. Cluster & ingress

GKE Ingress (external Application Load Balancer). In `ingress-patch.yaml`, leave
`ingressClassName` unset, delete the ingress-nginx lines, and add these annotations:

```yaml
kubernetes.io/ingress.class: gce              # gce-internal for an internal LB
networking.gke.io/v1beta1.FrontendConfig: simple-host-frontend
```

Then add a patch for the Service:
`cloud.google.com/backend-config: '{"default": "simple-host-backend"}'`.

```yaml
apiVersion: networking.gke.io/v1beta1
kind: FrontendConfig
metadata: {name: simple-host-frontend, namespace: simple-host}
spec:
  redirectToHttps: {enabled: true}
---
apiVersion: cloud.google.com/v1
kind: BackendConfig
metadata: {name: simple-host-backend, namespace: simple-host}
spec:
  timeoutSec: 300
  healthCheck: {type: HTTP, requestPath: /healthz}
```

- Install with `make install INGRESS=none`: a GKE cluster may have no
  IngressClass object, and `INGRESS=auto` would then add ingress-nginx.
- The load balancer does not cap the request body.
- Reserve a static global IP and name it with
  `kubernetes.io/ingress.global-static-ip-name`.

## 2. DNS & wildcard certificate

Cloud DNS `A` records for `<base>` and `*.<base>` point at the static IP.

A `ManagedCertificate` does not issue wildcards. Pick one of these:

- **cert-manager with Cloud DNS (works with Ingress).** Keep
  `cert-manager.io/cluster-issuer: letsencrypt-dns`. GKE Ingress serves the Secret
  named in `spec.tls`.

  ```yaml
  solvers:
    - dns01:
        cloudDNS: {project: <project-id>}
  ```

  Grant `roles/dns.admin` to the cert-manager ServiceAccount through Workload
  Identity Federation (principal
  `principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<project-id>.svc.id.goog/subject/ns/cert-manager/sa/cert-manager`).
- **Certificate Manager with DNS authorization.** This issues a Google-managed
  wildcard, but it attaches to the load balancer through a certificate map on a
  **Gateway** (`networking.gke.io/certmap`), not on an Ingress.

**Owner certificates (`*.<owner>.<base>`).** GKE Ingress makes one load balancer
per Ingress, so the per-owner Ingresses the reconciler creates would each get
their own address while `*.<base>` points at the static IP. Pick one:

- Serve through an in-cluster ingress controller behind one `LoadBalancer`
  Service on the static IP, with the cert-manager issuer above (or an internal CA
  issuer) in `OWNER_CERT_ISSUER`. Owner certificates are then automatic.
- Keep GKE Ingress, set `OWNER_CERTS=manual`, leave out the owner-hosts component,
  and add each owner's `*.<owner>.<base>` certificate Secret as another `spec.tls`
  entry on the install's own Ingress (served by SNI, within the load balancer's
  certificate limit), before that owner's first site.

## 3. Postgres

Cloud SQL for PostgreSQL 16 on a private IP. Run this as one command:

```
gcloud sql instances create <instance> --database-version=POSTGRES_16 --region=<region> --network=<vpc> --no-assign-ip --ssl-mode=ENCRYPTED_ONLY --server-ca-mode=GOOGLE_MANAGED_CAS_CA --backup-start-time=03:00 --enable-point-in-time-recovery --retained-transaction-log-days=7
```

- Encryption at rest: Cloud SQL always encrypts storage and backups (Google-managed
  key; `--disk-encryption-key` at creation for a customer-managed one).
- You can only set the server CA mode at creation. With `GOOGLE_MANAGED_CAS_CA`, the server
  certificate carries the instance's DNS name, which `verify-full` needs.
- DNS name: `gcloud sql instances describe <instance> --format='yaml(dnsName,dnsNames)'`.
  A private-services-access instance looks like `<uid>.<label>.<region>.sql-psa.goog`.
  Make it resolve to the private IP. Cloud SQL docs say you set up this resolution
  yourself, for example with a Cloud DNS private zone.
- CA: `gcloud sql ssl server-certs list --instance=<instance> --format='value(ca_cert.cert)' > deploy/overlays/byo/db-ca.crt`

```
DB_HOST=<dnsName without the trailing dot>
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

## 4. Bucket

Cloud Storage through its S3 XML API.

```
gcloud storage buckets create gs://<bucket> --location=<region> --uniform-bucket-level-access
gcloud storage buckets update gs://<bucket> --versioning --lifecycle-file=lifecycle.json
```

Put this rule in `lifecycle.json`:
`{"rule":[{"action":{"type":"Delete"},"condition":{"isLive":false,"daysSinceNoncurrentTime":30}}]}`.
Encryption at rest is on by default.

```
BACKUP_STORAGE_ENDPOINT=https://storage.googleapis.com
BACKUP_STORAGE_REGION=<region>
BACKUP_STORAGE_BUCKET=<bucket>
```

The server sets request checksums for GCS compatibility.

It is unverified whether the XML API accepts the `x-amz-server-side-encryption: AES256`
header that `BACKUP_SSE` always sends. Check that the first write succeeds.

## 5. Identity for pods

The server's S3 client cannot use Workload Identity Federation for GKE, because the AWS SDK
chain does not know it. Create an HMAC key for a service account that has
`roles/storage.objectUser` on the bucket:
`gcloud storage hmac create <sa-email>`. Put the access ID and secret in
`BACKUP_STORAGE_ACCESS_KEY_ID` / `BACKUP_STORAGE_SECRET_ACCESS_KEY`.

Use Workload Identity Federation for cert-manager (section 2).

## 6. OIDC provider notes

- A private cluster needs Cloud NAT (or a proxy) to reach the issuer on 443.
- **Okta:** set client authentication to "Client secret" with `client_secret_post`.
- **Entra ID:** single-tenant app, issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`.

## 7. Verify

```
kubectl -n simple-host rollout status deploy/simple-host
curl -fsS https://<base>/healthz
kubectl -n simple-host run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-api-key>
```
