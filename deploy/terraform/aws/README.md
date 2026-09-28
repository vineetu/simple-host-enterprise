# Simple Host Enterprise on AWS with Terraform

One Terraform root module that sets up everything in your AWS account:

- **A cluster**: a new EKS cluster (2 × `t3.medium` in private subnets of a
  new VPC, one NAT gateway), or your existing one (`create_cluster = false`).
- **Postgres**: RDS for PostgreSQL 16, encrypted, TLS forced, 7 days of
  point-in-time recovery, reachable only from the cluster's VPC.
- **The bucket**: S3 with versioning, a 30-day noncurrent-version lifecycle,
  SSE-S3, public access blocked, TLS-only policy.
- **Secrets**: the session signing key, the envelope key and both database
  passwords are generated here and kept in AWS Secrets Manager
  (`<name>/app`); the sign-in app's client secret goes to `<name>/oidc`.
  External Secrets Operator copies both into the `simple-host-secrets` Secret.
  Nobody types or sees them, and **Secrets Manager is the envelope key's
  escrow**: every site is readable only with it.
- **No access keys**: IAM roles for service accounts (IRSA) for the server
  (the bucket), External Secrets (the two secrets) and cert-manager (DNS-01).
- **Ingress and certificates**: Traefik behind a Network Load Balancer,
  cert-manager, a Route 53 zone for your address, and a Let's Encrypt issuer
  (DNS-01) for the address, `*.<address>`, and every owner's
  `*.<owner>.<address>`. Owner certificates are automatic.
- **Simple Host** at the pinned release digest (the `byo` overlay's shape,
  applied with kubectl), then it waits for the rollout.

Why Traefik and not the ALB (`docs/cloud/aws.md` section 1): every owner gets
a certificate of their own from cert-manager on an Ingress of its own, and the
ALB takes certificates from ACM only, which makes owner certificates manual.
Traefik serves certificates from Secrets for any number of Ingresses behind one
load balancer.

## The quick way: AWS CloudShell

https://simple-host.app/setup (Enterprise → AWS) asks a few questions and gives
one line to paste into AWS CloudShell. It runs `apply.sh` from this directory
at a pinned commit, after checking its checksum:

1. installs Terraform (and kubectl) if missing, checked against pinned checksums;
2. fetches this repository at that commit and writes `terraform.tfvars`;
3. keeps the Terraform state in `s3://<name>-tfstate-<account>-<region>`
   (created once: versioning, encryption, TLS only, lockfile locking), so running
   the line again, from any shell, continues where it stopped;
4. on an existing cluster, notices a cert-manager, External Secrets Operator or
   IAM OIDC provider already there and uses it instead of installing another;
5. asks once for the client secret (typed, not shown);
6. runs `terraform apply`, which shows the plan and waits for `yes`.

An AWS account on the free plan cannot launch the nodes or the database at
their default sizes; `apply.sh` says so before it starts. Upgrade the account's
plan first.

`--plan` shows the plan and changes nothing; `--yes` skips the confirmation;
`--destroy` removes what it made.

At the end it prints the **NS records** for your address. Add them at your
DNS provider (they delegate the address to the Route 53 zone; for a domain of
its own, set them as its name servers at the registrar). If a public Route 53
zone for the address already exists in the account, it is used and there is
nothing to add. Then:

```sh
curl -fsS https://<address>/readyz
```

prints `{"status":"ok"}`, and an admin signs in at `https://<address>/auth/login`.
Finish as `INSTALL.md` sections 8 and 9 say (HUMAN STEP D and `make smoke`).

## From your own pipeline

```sh
cd deploy/terraform/aws
terraform init -backend-config="bucket=<state bucket>" -backend-config="key=<address>/terraform.tfstate" -backend-config="region=<region>" -backend-config="use_lockfile=true"
TF_VAR_oidc_client_secret=<from your secret store> terraform apply -var-file=terraform.tfvars
```

The runner needs Terraform 1.10 or later, the AWS CLI (the Kubernetes
providers and kubectl sign in with `aws eks get-token`) and kubectl, with an
identity that can create the resources above and, on an existing cluster,
cluster-admin. The client secret is needed on the first apply only; later runs
keep the stored one (change it with
`aws secretsmanager put-secret-value --secret-id <name>/oidc --secret-string file:///dev/stdin`,
then `kubectl -n simple-host rollout restart deploy/simple-host`).

## Variables

`terraform.tfvars` from the setup page holds `create_cluster`, `cluster_name`
(existing cluster), `region`, `base_domain`, `admin_emails`,
`allowed_email_domains` (with Google), `oidc_issuer`, `oidc_client_id` and,
from its Advanced step, `extra_config` (more `config.env` settings,
`docs/configuration.md`). Everything else has a default in `variables.tf`:
`name` (resource prefix, `simple-host`), `image_digest` (the pinned release),
`node_size` and `node_count` (used when the cluster is created; scale it later in EKS), `db_size`, `db_backup_days` (7), `protect_data` (deletion protection on
the database and bucket; `true`), `dns_zone_id`, `letsencrypt_email`, `tags`.

## Upgrades, changes, removal

- **Upgrade**: set `image_digest` to the new release's verified digest
  (`INSTALL.md` section 3) and apply. Pods restart when their configuration
  changes.
- **A setting**: add it to `extra_config` and apply.
- **Remove**: `bash apply.sh ... --destroy`, or `terraform destroy`. With
  `protect_data = true` (the default) the database refuses deletion and the
  bucket keeps its objects; take the data you need first (`docs/uninstall.md`),
  set `protect_data = false`, apply, then destroy. Delete the NS records at
  your DNS provider too.
