# GKE

DRAFT — reconcile after Phase 4 and 7.

Same six pieces as `docs/cloud/aws.md`; nothing here is compiled into the
application. Every value below is a Kubernetes manifest field or a value in
`deploy/overlays/byo/config.env`/`secrets.env`.

## 1. Storage class with encryption

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simple-host-encrypted
provisioner: pd.csi.storage.gke.io
parameters:
  type: pd-ssd
  # Omit disk-encryption-kms-key to use Google-managed encryption, which is
  # on by default for every persistent disk regardless. Set it only when a
  # reviewer specifically requires a customer-managed key (CMEK).
  # disk-encryption-kms-key: projects/<project>/locations/<region>/keyRings/<ring>/cryptoKeys/<key>
allowVolumeExpansion: true
```

Google-managed encryption is on for every persistent disk by default, with
no configuration; CMEK is the addition for a reviewer who specifically
requires customer control over the key, not a baseline you must configure
to get encryption at all.

## 2. Managed Postgres with TLS and PITR

Cloud SQL for PostgreSQL 16:

- `require_ssl` (or the newer `ssl_mode` set to a verify- variant) enabled
  on the instance, matching this package's own refusal of anything but
  `sslmode=verify-full`.
- CMEK optional, same reasoning as the disk above — see the grant note.
- Point-in-time recovery enabled (`--enable-point-in-time-recovery`), with
  your retention window sized to your recovery point objective (design
  9.1's own documented default expectation is 7 days).

Download the instance's server CA certificate and place it where
`docs/install.md` section 5 step 3 expects
(`deploy/overlays/byo/db-ca.crt`), then in `config.env`:

```
DB_HOST=<instance-connection-name-or-private-ip>
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

The state document lives only in Postgres, so this PITR window is its
whole backup story.

## 3. Bucket with default encryption

A Cloud Storage bucket, reached through its S3-compatible XML API (this
package's backup client speaks the S3 API; it does not use the native
Google Cloud Storage API):

- Uniform bucket-level access enabled.
- CMEK optional on the bucket (again, the grant note below); Google-managed
  encryption otherwise applies by default with no configuration.
- An HMAC key pair for the service account the workload uses, since the
  S3-compatible XML API authenticates with HMAC credentials, not a bearer
  token from workload identity directly.

```
BACKUP_STORAGE_ENDPOINT=https://storage.googleapis.com
BACKUP_STORAGE_REGION=<region>
BACKUP_STORAGE_BUCKET=<your-bucket>
BACKUP_STORAGE_PREFIX=backups/
BACKUP_SSE=AES256
```

Set `BACKUP_STORAGE_ACCESS_KEY_ID`/`BACKUP_STORAGE_SECRET_ACCESS_KEY` from the HMAC
key pair — unlike AWS's IRSA path, Cloud Storage's S3-compatible API has no
direct workload-identity equivalent, so this is the one place a GKE
install keeps a static credential in the Secret rather than relying on an
injected identity.

## 4. Ingress / load balancer

Either GKE's native Ingress (backed by Google Cloud Load Balancing) or
ingress-nginx behind a `Service` of type `LoadBalancer`, fronting the same
`deploy/base/ingress.yaml` object this package ships. TLS terminates at
this layer.

## 5. DNS wildcard

A Cloud DNS record for `<base>` and `*.<base>` pointing at the load
balancer's IP (an `A` record; Cloud DNS has no alias-to-load-balancer
shortcut the way Route 53 does, so this is a plain `A` record to the
reserved static IP you should give the load balancer).

## 6. Certificate via cert-manager, DNS-01

cert-manager's Cloud DNS solver is built in:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-dns
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: platform-team@yourcompany.com
    privateKeySecretRef:
      name: letsencrypt-dns-account-key
    solvers:
      - dns01:
          cloudDNS:
            project: <your-project-id>
            # Uses Workload Identity (see the grant note below) rather
            # than a downloaded service-account key file, the same
            # preference as avoiding a static key pair anywhere a
            # workload identity is available.
```

Alternatively, Google-managed certificates cover the base host alone (they
do not support wildcards), which is why cert-manager with the Cloud DNS
solver is what actually covers `*.<base>` and is the path this package
documents.

Reference the issuer's name from
`deploy/overlays/byo/ingress-patch.yaml`'s `cert-manager.io/cluster-issuer`
annotation. **Scope the DNS Administrator role narrowly**: bind it to the
one managed zone cert-manager needs, not project-wide DNS admin — the same
least-privilege principle `agent-deploy`'s DNS tooling applies at its own
seam for a non-Kubernetes install, where every write is refused unless it
is scoped to an explicitly allowed zone or subtree.

## 7. The grant note a reviewer asks about

**A CMEK key's IAM policy must name the service agent that creates the
encrypted resource, not just your workload's own identity.** Google
provisions a separate, project-scoped service agent for each managed
service that can use CMEK — the Compute Engine service agent
(`service-<project-number>@compute-system.iam.gserviceaccount.com`) for a
CMEK persistent disk, and the Cloud SQL service agent
(`service-<project-number>@gcp-sa-cloud-sql.iam.gserviceaccount.com`) for a
CMEK Cloud SQL instance — and each needs the `Cloud KMS CryptoKey
Encrypter/Decrypter` role on the key, granted separately from whatever
Workload Identity binding lets your pod's own service account reach the
database or the bucket. Granting only your workload's identity and
omitting the relevant service agent is the GKE analogue of the AWS
grant-vs-key-policy gap in `docs/cloud/aws.md` section 7, and is exactly
what a reviewer who has hit this before will ask about first.
