# AWS (EKS)

Verified end to end on this cloud, 2026-09-28:
- **By hand, with the ALB and ACM:** a real sign-in, publishing, sharing and `make smoke`
  (46 of 46).
- **The Terraform quick path below:** a new cluster and an existing one, from AWS CloudShell
  and from a Linux shell, to `/readyz` over a Let's Encrypt certificate, the sign-in redirect,
  an owner certificate, an upgrade and a settings change rolling the pods, and runs stopped
  halfway and picked up again. Not with a real sign-in, publish or `make smoke`.

Fills the sections in [README.md](README.md).

**Quick path.** https://simple-host.app/setup (Enterprise → AWS) gives one line
for AWS CloudShell that runs the Terraform module in
[`deploy/terraform/aws`](../../deploy/terraform/aws/README.md): a new EKS
cluster or yours, RDS, S3, Secrets Manager, IRSA, cert-manager, Traefik behind
an NLB, a Route 53 zone with Let's Encrypt certificates (owner certificates
included), and Simple Host. It follows sections 3 to 6 below. A new cluster's VPC has
one NAT gateway (private nodes), about $33 a month plus $0.045 per GB it carries
(us-east-2 list prices). For the ingress
it uses Traefik instead of the ALB, so owner certificates stay automatic
(section 2). The sections below are for doing it by hand.

## 1. Cluster & ingress

Pick the ingress by how owner certificates are issued (section 2): every person
or team that publishes gets its own `*.<owner>.<base>` certificate.

**Recommended: an in-cluster controller behind a Network Load Balancer.** Owner
certificates are then automatic (cert-manager writes them into Secrets and the
controller serves them), whatever the number of owners. This is what the quick
path installs: Traefik (Helm chart `traefik/traefik`) with its own IngressClass,
its Service of type `LoadBalancer` annotated
`service.beta.kubernetes.io/aws-load-balancer-type: nlb` and
`externalTrafficPolicy: Local` (the NLB keeps each person's address), the
`websecure` entry point's read and write timeouts at 300 s, and `web` redirected
to `websecure`. Traefik has no body-size cap. In `ingress-patch.yaml` set
`spec.ingressClassName` to that class and delete the ingress-nginx lines.
ingress-nginx is retired upstream (maintenance ended March 2026).

**The ALB (AWS Load Balancer Controller) with ACM** works for the base address,
but owner certificates are then a manual job per person (section 2): an ACM
certificate, its validation record, two DNS records and an ingress edit, before
that person's first publish, with a default limit of 25 certificates per ALB
listener. Use it only for a handful of owners. Install the controller with its
IAM role (eksctl), then its chart:

```sh
eksctl create iamserviceaccount --cluster <cluster> --region <region> --namespace kube-system --name aws-load-balancer-controller --role-name <cluster>-alb-controller --attach-policy-arn arn:aws:iam::<account>:policy/AWSLoadBalancerControllerIAMPolicy --approve
```

(the policy document is the controller release's `docs/install/iam_policy.json`;
with an eksctl config file, `wellKnownPolicies: {awsLoadBalancerController: true}`
on the service account does the same), then

```sh
helm install aws-load-balancer-controller aws-load-balancer-controller --repo https://aws.github.io/eks-charts -n kube-system --set clusterName=<cluster> --set serviceAccount.create=false --set serviceAccount.name=aws-load-balancer-controller --set region=<region> --set vpcId=<vpc-id>
```

In `ingress-patch.yaml`, set `spec.ingressClassName: alb`, delete the ingress-nginx
and cert-manager lines, and use:

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
- The ALB path needs no cert-manager and no DNS-01 issuer: skip those rows of
  INSTALL.md section 1 and HUMAN STEP C. `make install` still installs cert-manager
  when it is missing; it does no harm, but it takes 3 pods (count them on small nodes).

**`TRUSTED_PROXY_CIDRS`.** It names the addresses that connect to the pods and may
pass on the client's address. With the ALB there are no proxy pods: the ALB's own
addresses in the VPC connect, so use the VPC CIDR (or the ALB subnets' CIDRs). With
Traefik on the VPC CNI its pods have VPC addresses, so the VPC CIDR is right there too.

## 2. DNS & wildcard certificate

**With an in-cluster controller (recommended): cert-manager, Route 53 DNS-01 and
Let's Encrypt.** Put `<base>` in a Route 53 hosted zone of its own and delegate it
from wherever the parent domain lives (NS records at that DNS provider, or the
domain's name servers at the registrar). Alias `A` records for `<base>` and `*.<base>`
point at the NLB. One ClusterIssuer then signs the base certificate and every
owner's, with nothing to do per person:

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
          route53:
            region: <region>
            hostedZoneID: <zone-id>
            role: <role-arn>
            auth: {kubernetes: {serviceAccountRef: {name: <service-account>}}}
```

The role trusts that ServiceAccount through IRSA (or put the role on the
cert-manager ServiceAccount itself and drop `role` and `auth`). cert-manager needs
the right to request its token: a Role in cert-manager's namespace allowing `create`
on `serviceaccounts/token` for that name, bound to cert-manager's ServiceAccount.
IAM policy: `route53:GetChange` on `arn:aws:route53:::change/*`;
`route53:ChangeResourceRecordSets` and `route53:ListResourceRecordSets` on
`arn:aws:route53:::hostedzone/<zone-id>`; `route53:ListHostedZonesByName` on `*`.
Set `OWNER_CERT_ISSUER` to the issuer's name.

- **Let's Encrypt's limits.** At most 50 new certificates per registered domain
  per week (shared with anything else on that domain that uses Let's Encrypt), and
  there is one per owner. With more than about 50 people publishing in the first
  week, the rest are served at `<owner>.<base>/<site>/` until theirs arrives. For a
  big rollout, sign owner certificates with your own CA: a cert-manager CA
  ClusterIssuer (INSTALL.md, "Site addresses") named in `OWNER_CERT_ISSUER` (in the
  quick path, `extra_config = { OWNER_CERT_ISSUER = "<issuer>" }`), and keep Let's
  Encrypt for the base address.

- **CAA.** If `<base>` or a parent domain has CAA records, the issuing CA must be
  in them. A CAA record in the `<base>` zone itself (`0 issue "letsencrypt.org"`
  and `0 issuewild "letsencrypt.org"`) ends the lookup there, so a parent's CAA no
  longer matters.
- **Keep the records in Route 53.** While cert-manager proves an owner's
  certificate, a record sits at `_acme-challenge.<owner>.<base>`. Route 53 still
  answers the `*.<base>` wildcard for `<owner>.<base>` and the names below it
  (checked 2026-09-28); many other DNS services do not, and the owner's addresses
  stop resolving for that minute and the negative-cache time after it.

**With the ALB: ACM.** Request one ACM certificate for `<base>` with `*.<base>` as
a second name, validate it by DNS, and put its ARN in `certificate-arn`.

- **CAA.** If `dig CAA <base>` (or its parent) returns records, add
  `0 issue "amazon.com"` and `0 issuewild "amazon.com"` first. A certificate that
  failed on CAA cannot be retried: delete it and request a new one, after the
  negative-cache time has passed.
- **DNS.** Route 53: alias `A`/`AAAA` records for `<base>` and `*.<base>` pointing
  at the ALB. At another DNS provider, `<base>` needs its ALIAS, ANAME or CNAME
  flattening (a CNAME cannot sit at a name that also has CAA or NS records); `*.<base>`
  is a plain CNAME to the ALB's name.
- **Owner certificates (`*.<owner>.<base>`).** The ALB takes certificates from ACM
  only, never from the Secrets cert-manager writes, and an Ingress without a shared
  `alb.ingress.kubernetes.io/group.name` gets its own ALB, so the reconciler's
  automatic mode does not fit. Set `OWNER_CERTS=manual`, leave out the owner-hosts
  component, and for each owner, **before** that owner's first site:
  1. add DNS records for `<owner>.<base>` and `*.<owner>.<base>` pointing at the ALB
     (as for `<base>`). The next step's validation record puts a name under
     `<owner>.<base>`, and from then on the `*.<base>` record no longer answers for
     `<owner>.<base>` or anything below it; without these two records the owner's
     addresses do not resolve (and a resolver may remember that for the zone's
     negative TTL);
  2. request an ACM certificate for `*.<owner>.<base>` and add its validation record
     (keep it: renewal needs it);
  3. add the certificate's ARN to `certificate-arn` (comma-separated).
  The owner's username is known only after their first sign-in. With
  `OWNER_CERTS=manual` every owner counts as ready, so someone who publishes before
  this is done gets an address that does not resolve.

## 3. Postgres

RDS for PostgreSQL 16.

- TLS: `rds.force_ssl` defaults to `1` on PostgreSQL 15 and later; keep it on.
- CA bundle: `curl -fsSo deploy/overlays/byo/db-ca.crt https://truststore.pki.rds.amazonaws.com/<region>/<region>-bundle.pem`
- PITR: automated backups on, backup retention 7+ days. An account on AWS's free
  plan refuses more than 1 day (`FreeTierRestrictionError`); upgrade its plan for a
  real install.
- Encryption at rest: create the instance with `--storage-encrypted` (optionally
  `--kms-key-id <key>`); `aws rds create-db-instance` leaves it off by default and it
  cannot be turned on later without a snapshot copy and restore.
- **Owner password: do not use `--manage-master-user-password`** for the role the
  install connects as. It turns on rotation every 7 days, and `DB_PASSWORD` in
  `secrets.env` is copied once, so within a week the migrate init container (every
  pod start) and the prune job fail to sign in. Create the instance with a throwaway
  master password (`--master-user-password` from a file or prompt, never typed on a
  shared machine's command line), then set the real one from `psql` with
  `\password simplehost`, which sends only a hash; that is the generated
  `DB_PASSWORD` (INSTALL.md section 4). If you do use a managed master password,
  turn its rotation off (`aws secretsmanager cancel-rotate-secret --secret-id <arn>`)
  or have External Secrets sync it into the Secret. The quick path generates the
  password with Terraform, keeps it in Secrets Manager without rotation, and syncs it.

```
DB_HOST=<instance>.<id>.<region>.rds.amazonaws.com
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

Use the instance (or cluster) endpoint as `DB_HOST`; a CNAME of your own fails
`verify-full`. When the database lives in the cluster's VPC, delete its security
group and subnet group before the cluster at removal time, or they hold up the
VPC's deletion.

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

The role needs `s3:ListBucket` (and, for the startup versioning check, `s3:GetBucketVersioning`) on the bucket, and `s3:GetObject`, `s3:PutObject` and
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
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-full-api-key>
```
