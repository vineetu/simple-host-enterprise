# AKS

DRAFT — reconcile after Phase 4 and 7.

Same six pieces as `docs/cloud/aws.md` and `docs/cloud/gke.md`; nothing
here is compiled into the application. Every value below is a Kubernetes
manifest field or a value in `deploy/overlays/byo/config.env`/`secrets.env`.

## 1. Storage class with encryption

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simple-host-encrypted
provisioner: disk.csi.azure.com
parameters:
  skuName: Premium_LRS
  # Omit diskEncryptionSetID to use platform-managed keys, which encrypt
  # every managed disk by default. Set it only when a reviewer specifically
  # requires a customer-managed key (CMK) — see the grant note in section 7.
  # diskEncryptionSetID: /subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/diskEncryptionSets/<des>
allowVolumeExpansion: true
```

Platform-managed keys encrypt every Azure managed disk by default, with no
configuration; a `diskEncryptionSetID` is the addition for a reviewer who
specifically requires customer control over the key.

## 2. Managed Postgres with TLS and PITR

Azure Database for PostgreSQL Flexible Server, PostgreSQL 16:

- `require_secure_transport` (SSL enforcement) on, matching this package's
  own refusal of anything but `sslmode=verify-full`.
- CMK optional via a customer-managed key on the server (see the grant
  note); platform-managed encryption applies by default otherwise.
- Automated backups with point-in-time restore enabled, retention sized to
  your recovery point objective (design 9.1's own documented default
  expectation is 7 days).

Download Azure's PostgreSQL server CA bundle and place it where
`docs/install.md` section 5 step 3 expects
(`deploy/overlays/byo/db-ca.crt`), then in `config.env`:

```
DB_HOST=<your-server>.postgres.database.azure.com
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

The state document lives only in Postgres, so this PITR window is its
whole backup story.

## 3. Bucket with default encryption

Azure Blob Storage has no S3-compatible API, and this package's backup
client speaks the S3 API only. Azure is the one platform in `docs/cloud/`
where backups need a piece you would not otherwise run. Decide this before
you install, not after.

**Do not reach for a MinIO gateway.** MinIO deprecated gateway mode in
2022 and removed it; guides that still recommend `minio gateway azure` are
describing a binary you can no longer get. Two answers that work:

- **MinIO as a server, on Azure managed disks.** Not a translation layer in
  front of Blob Storage — a real object store of its own, on a
  PersistentVolumeClaim with an encrypting StorageClass. The package ships
  it as `deploy/components/minio`, so this is one line in your
  kustomization. The trade is that backups then live on a disk in the same
  cluster rather than in a separate, independently durable service, which
  is a weaker backup story than the other clouds get. If this is where you
  land, replicate that volume or sync the bucket onward.
- **An S3-to-Blob proxy**, such as `s3proxy`, which does what MinIO's
  gateway used to: presents an S3 endpoint and stores objects in a Blob
  container. This keeps backups in Blob Storage, which is what you probably
  wanted, at the cost of another component to patch and keep running.

If neither is acceptable, Azure is the wrong platform for this package
today. Saying so is cheaper than discovering it at the first restore.

Either way, the storage account itself should have encryption at rest on
(the default for every Azure Storage account) with a customer-managed key
in Key Vault if your reviewer requires one:

```
BACKUP_STORAGE_ENDPOINT=https://<your-minio-service-or-s3proxy>
BACKUP_STORAGE_REGION=<region-your-front-expects, often a placeholder>
BACKUP_STORAGE_BUCKET=<container-name>
BACKUP_STORAGE_PREFIX=backups/
BACKUP_SSE=AES256
```

Whether `BACKUP_SSE`'s header is honored depends entirely on what the
S3-compatible service does with it; confirm this
against whichever front you choose before relying on it as a control, the
same way Phase 5 found that self-hosted MinIO needs its own KMS backend
configured before it accepts the header at all (`docs/install.md`
section 7).

## 4. Ingress / load balancer

Either the Application Gateway Ingress Controller (AGIC) or ingress-nginx
behind a `Service` of type `LoadBalancer` (an Azure Load Balancer), fronting
the same `deploy/base/ingress.yaml` object this package ships. TLS
terminates at this layer.

## 5. DNS wildcard

An Azure DNS record for `<base>` and `*.<base>` pointing at the load
balancer's public IP (an `A` record to the reserved static IP you should
give the load balancer).

## 6. Certificate via cert-manager, DNS-01

cert-manager's Azure DNS solver is built in:

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
          azureDNS:
            subscriptionID: <your-subscription-id>
            resourceGroupName: <the-dns-zone-resource-group>
            hostedZoneName: <your-zone>
            environment: AzurePublicCloud
            # Uses a workload identity federated credential (see the grant
            # note below) rather than a downloaded service principal
            # secret, the same preference as avoiding a static credential
            # anywhere a managed identity is available.
```

Reference the issuer's name from
`deploy/overlays/byo/ingress-patch.yaml`'s `cert-manager.io/cluster-issuer`
annotation. **Scope the role assignment narrowly**: `DNS Zone Contributor`
on the one DNS zone resource, not `Contributor` at the subscription or
resource-group level — the same least-privilege principle `agent-deploy`'s
DNS tooling applies at its own seam for a non-Kubernetes install, where
every write is refused unless it is scoped to an explicitly allowed zone
or subtree.

## 7. The grant note a reviewer asks about

**A managed disk's or PostgreSQL server's CMK is not accessed by the AKS
cluster's own managed identity — it is accessed by a separate identity tied
to the encrypting resource itself, which must be granted Key Vault access
independently.** For a CMK-encrypted managed disk, Azure creates a Disk
Encryption Set object whose own system-assigned managed identity must be
granted `Key Vault Crypto Service Encryption User` (or an equivalent access
policy) on the Key Vault holding the key — granting your AKS cluster's
kubelet identity or workload identity access to the vault does nothing for
disk encryption. Azure Database for PostgreSQL Flexible Server's CMK
follows the same shape: the server's own identity, not the application's,
needs the Key Vault grant. This is the AKS analogue of the AWS
grant-vs-key-policy gap in `docs/cloud/aws.md` section 7 and the GKE
service-agent gap in `docs/cloud/gke.md` section 7, and is exactly what a
reviewer who has hit this before will ask about first.
