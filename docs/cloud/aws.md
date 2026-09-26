# AWS (EKS)

Verified end to end on this cloud: no.

Fills the sections in [README.md](README.md).

## 1. Cluster & ingress

Use the AWS Load Balancer Controller (ALB). In `ingress-patch.yaml`, set
`spec.ingressClassName: alb`, delete the ingress-nginx and cert-manager lines, and use:

```yaml
alb.ingress.kubernetes.io/scheme: internal   # or internet-facing
alb.ingress.kubernetes.io/target-type: ip
alb.ingress.kubernetes.io/healthcheck-path: /healthz
alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=300
alb.ingress.kubernetes.io/certificate-arn: arn:aws:acm:<region>:<account>:certificate/<id>
alb.ingress.kubernetes.io/listen-ports: '[{"HTTP": 80}, {"HTTPS": 443}]'
alb.ingress.kubernetes.io/ssl-redirect: "443"
```

- `scheme`: `internal` when only your network (VPN, peered VPC) should reach it;
  `internet-facing` when people sign in from anywhere. Pick it on purpose: the
  controller defaults to `internal`.
- An ALB does not cap the request body, so there is no body-size setting.
- Alternative: an ingress controller behind an NLB. ingress-nginx is retired upstream
  (maintenance ended March 2026), so prefer the ALB.

## 2. DNS & wildcard certificate

**ACM (recommended with the ALB):** request one ACM certificate for `<base>` with
`*.<base>` as a second name, validate it by DNS, and put its ARN in `certificate-arn`.
You do not need cert-manager.

Route 53: alias `A`/`AAAA` records for `<base>` and `*.<base>` pointing at the ALB.

**cert-manager instead** (for example, behind an NLB): Route 53 DNS-01, with credentials from IRSA
or EKS Pod Identity on the cert-manager ServiceAccount.

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: letsencrypt-dns}
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: <platform-team-address>
    privateKeySecretRef: {name: letsencrypt-dns-account-key}
    solvers:
      - dns01:
          route53: {region: <region>, hostedZoneID: <zone-id>}
```

IAM policy for that role: `route53:GetChange` on `arn:aws:route53:::change/*`;
`route53:ChangeResourceRecordSets` and `route53:ListResourceRecordSets` on
`arn:aws:route53:::hostedzone/<zone-id>`; `route53:ListHostedZonesByName` on `*`.

## 3. Postgres

RDS for PostgreSQL 16.

- TLS: `rds.force_ssl` defaults to `1` on PostgreSQL 15 and later; keep it on.
- CA bundle: `curl -fsSo deploy/overlays/byo/db-ca.crt https://truststore.pki.rds.amazonaws.com/<region>/<region>-bundle.pem`
- PITR: automated backups on, backup retention 7+ days.
- Encryption at rest: create the instance with `--storage-encrypted` (optionally
  `--kms-key-id <key>`); `aws rds create-db-instance` leaves it off by default and it
  cannot be turned on later without a snapshot copy and restore. Owner password: use
  `--manage-master-user-password` rather than passing one on the command line.

```
DB_HOST=<instance>.<id>.<region>.rds.amazonaws.com
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

Use the instance (or cluster) endpoint as `DB_HOST`; a CNAME of your own fails
`verify-full`.

## 4. Bucket

S3, versioning on, with a lifecycle rule that uses `NoncurrentVersionExpiration`
(`NoncurrentDays` = your retention). Default encryption SSE-S3 or SSE-KMS. Also add a
bucket policy that denies `aws:SecureTransport = false`.

```
BACKUP_STORAGE_ENDPOINT=https://s3.<region>.amazonaws.com
BACKUP_STORAGE_REGION=<region>
BACKUP_STORAGE_BUCKET=<bucket>
BACKUP_SSE=AES256
# SSE-KMS: BACKUP_SSE=aws:kms and BACKUP_SSE_KEY_ID=arn:aws:kms:<region>:<account>:key/<id>
```

The bucket holds the site data, so you do not need the EBS CSI driver or a StorageClass for it.

## 5. Identity for pods

Leave `BACKUP_STORAGE_ACCESS_KEY_ID` and `BACKUP_STORAGE_SECRET_ACCESS_KEY` out of
`secrets.env`. The server then uses the AWS SDK default credential chain, which reads IRSA
and EKS Pod Identity. The code requires both keys set or neither.

- **IRSA:** annotate the `simple-host` ServiceAccount with `eks.amazonaws.com/role-arn: <role-arn>`.
- **Pod Identity:** create a pod identity association for `simple-host/simple-host`.

The role needs `s3:ListBucket` on the bucket, and `s3:GetObject`, `s3:PutObject` and
`s3:DeleteObject` on `<bucket>/*`. With SSE-KMS, it also needs `kms:GenerateDataKey` and `kms:Decrypt` on the key.

## 6. OIDC provider notes

- The pods need egress on 443 to the issuer (NAT gateway or proxy for private subnets).
- **Okta:** set the app's client authentication to "Client secret" with
  `client_secret_post`. Do not use `private_key_jwt`.
- **Entra ID:** single-tenant app, issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`.

## 7. Verify

```
kubectl -n simple-host rollout status deploy/simple-host
curl -fsS https://<base>/healthz
kubectl -n simple-host run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-api-key>
```
