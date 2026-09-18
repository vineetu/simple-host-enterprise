# AWS

DRAFT — reconcile after Phase 4 and 7.

This is the worked example (design 10.3), because AWS is where the source
instance ran. The package is platform-agnostic; nothing here is compiled
into the application, and every value below is a Kubernetes manifest field
or a value in `deploy/overlays/byo/config.env`/`secrets.env`, not a code
path. The core needs exactly six things from a platform — everything else
(key policies, snapshot lifecycle, IAM boundaries beyond the one grant note
below) is your platform team's own operational decision, not the
software's.

## 1. Storage class with encryption

An encrypting `gp3` `StorageClass` for the site-data PVC:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simple-host-encrypted
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  encrypted: "true"
  # Omit kmsKeyId to use the AWS-managed EBS key; set it to use a
  # customer-managed key. See the grant note in section 7 before doing so.
  # kmsKeyId: arn:aws:kms:us-east-1:111111111111:key/REPLACE_ME
allowVolumeExpansion: true
```

Reference this StorageClass from `deploy/base/pvc.yaml` (or an overlay
patch over it) before the PVC is first created — the storage class of an
existing PVC cannot be changed in place.

## 2. Managed Postgres with TLS and PITR

RDS for PostgreSQL 16, minimum:

- `StorageEncrypted: true`, with a customer-managed KMS key if your
  reviewer requires one (again, see the grant note).
- A parameter group that forces TLS (`rds.force_ssl = 1`), matching this
  package's own refusal of anything but `sslmode=verify-full`.
- `BackupRetentionPeriod` set for the PITR window your recovery point
  objective needs (7 days is this package's own documented default
  expectation — design 9.1 — raise it if your requirement is longer).
- Automated backups enabled, which is what PITR requires on RDS.

Download the RDS CA bundle and place it where `docs/install.md` section 5
step 3 expects (`deploy/overlays/byo/db-ca.crt`), then set in
`config.env`:

```
DB_HOST=<your-instance>.<region>.rds.amazonaws.com
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

The state document — the per-site JSON store — lives only in Postgres, so
PITR here is its entire backup story; nothing else in this package copies
it anywhere else.

## 3. Bucket with default encryption

An S3 bucket:

- Default server-side encryption on (SSE-S3 or SSE-KMS); matches this
  package's own `BACKUP_SSE` header sent on every `PutObject`, which is
  belt-and-braces on top of the bucket default, not a substitute for it.
- A bucket policy denying non-TLS requests (`aws:SecureTransport: false`
  → `Deny`).
- A lifecycle rule on the backup prefix (`BACKUP_STORAGE_PREFIX`, default
  `backups/`) for your retention window.

```
BACKUP_STORAGE_ENDPOINT=https://s3.<region>.amazonaws.com
BACKUP_STORAGE_REGION=<region>
BACKUP_STORAGE_BUCKET=<your-bucket>
BACKUP_STORAGE_PREFIX=backups/
BACKUP_SSE=AES256
# or, with a customer-managed KMS key:
# BACKUP_SSE=aws:kms
# BACKUP_SSE_KEY_ID=arn:aws:kms:<region>:<account>:key/<key-id>
```

Leave `BACKUP_STORAGE_ACCESS_KEY_ID`/`BACKUP_STORAGE_SECRET_ACCESS_KEY` unset to use
IRSA (IAM Roles for Service Accounts) instead of a static key pair, which
is the preferred path on EKS; the SDK's default credential chain picks up
the role bound to the pod's service account automatically.

## 4. Ingress / load balancer

The AWS Load Balancer Controller (or ingress-nginx behind an NLB, if your
platform team already standardizes on it) fronting the `Ingress` object
`deploy/base/ingress.yaml` composes. Either way, the wildcard TLS
certificate below terminates at this layer; the application itself never
sees TLS.

## 5. DNS wildcard

A Route 53 record for `<base>` and `*.<base>` pointing at the load
balancer's DNS name (an `A`/`AAAA` alias for an ALB/NLB, since Route 53
supports aliasing directly to an AWS load balancer without a static IP).

## 6. Certificate via cert-manager, DNS-01

A wildcard certificate cannot be issued over HTTP-01 — cert-manager's
Route53 DNS-01 solver is built in, no external webhook needed:

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
          route53:
            region: <region>
            hostedZoneID: <your-hosted-zone-id>
            # Uses IRSA (see the grant note below) rather than a static key
            # pair, the same preference as the backup bucket in section 3.
```

Reference this issuer's name from `deploy/overlays/byo/ingress-patch.yaml`'s
`cert-manager.io/cluster-issuer` annotation. **Scope the IRSA role narrowly**:
`route53:ChangeResourceRecordSets` and `route53:ListResourceRecordSets` on
the one hosted zone, `route53:GetChange` and `route53:ListHostedZones` more
broadly (the API requires them unscoped to a zone) — never a role with
write access to every zone in the account. This is the same principle
`agent-deploy`'s DNS tooling applies at the script layer for a
non-Kubernetes install: least-privilege, scoped to the one zone or subtree
a given credential should ever touch, enforced at the one seam every
write goes through rather than trusted to each caller.

## 7. The grant note a reviewer asks about

**EBS and RDS decrypt through grants held by the creating principal, not
just the resource's own IAM policy.** If you use a customer-managed KMS
key for the StorageClass (section 1) or RDS (section 2), the key's policy
must explicitly name the principal that *creates* the encrypted resource —
the EBS CSI driver's IAM role for a PVC, or the principal RDS uses
internally for an encrypted instance — via a key policy statement granting
`kms:CreateGrant` with the `kms:GrantIsForAWSResource` condition, not
merely a policy that grants your own workload's role `kms:Decrypt`. Naming
only the workload's own role and omitting the creating principal is the
single most common reason a customer-managed key silently breaks volume or
instance creation, and it is exactly what a reviewer with prior AWS KMS
experience will ask about first.
