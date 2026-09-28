# Simple Host Enterprise on AWS with Terraform

One Terraform root module that sets up everything in your AWS account:

- **A cluster**: a new EKS cluster (2 × `t3.medium` in private subnets of a
  new VPC, with one NAT gateway), or your existing one (`create_cluster = false`).
- **Postgres**: RDS for PostgreSQL 16, encrypted, TLS forced, 7 days of
  point-in-time recovery, reachable only from the cluster's VPC. Single-AZ;
  turn on Multi-AZ in RDS if you need it.
- **The bucket**: S3 with versioning, a 30-day noncurrent-version lifecycle,
  SSE-S3, public access blocked, TLS-only policy.
- **Secrets**: the session signing key, the envelope key and both database
  passwords are generated here and kept in AWS Secrets Manager
  (`<name>/app`); the sign-in app's client secret goes to `<name>/oidc`.
  External Secrets Operator copies both into the `simple-host-secrets` Secret.
  Nobody types or sees them, and **Secrets Manager is the envelope key's
  escrow**: every site is readable only with it. They are also in the
  Terraform state, which stays in an encrypted, private bucket of yours.
- **No access keys**: IAM roles for service accounts (IRSA) for the server
  (the bucket), External Secrets (the two secrets) and cert-manager (DNS-01).
- **Ingress and certificates**: Traefik behind a Network Load Balancer,
  cert-manager, a Route 53 zone for your address, and a Let's Encrypt issuer
  (DNS-01) for the address, `*.<address>`, and every owner's
  `*.<owner>.<address>`. Owner certificates are automatic.
- **Simple Host** at the pinned release digest (the `byo` overlay's shape,
  applied with kubectl), then it waits for the rollout.

`<name>` is `sh-` and 8 characters of the address's hash (set `name` to
choose it), so two installs in one account and region never share a name.

Why Traefik and not the ALB (`docs/cloud/aws.md` section 1): every owner gets
a certificate of their own from cert-manager on an Ingress of its own, and the
ALB takes certificates from ACM only, which makes owner certificates manual.
Traefik serves certificates from Secrets for any number of Ingresses behind one
load balancer.

What is pinned: Terraform and kubectl by sha256, the providers by the
committed `.terraform.lock.hcl`, the two registry modules by exact version
(vpc 6.7.3, eks 21.26.0), the Helm charts by version (cert-manager v1.21.2,
external-secrets 2.11.0, traefik 41.6.0; Helm has no chart digest, and the
charts pull their images by tag), and Simple Host by image digest.

## The quick way: AWS CloudShell

https://simple-host.app/setup (Enterprise → AWS) asks a few questions and gives
one line to paste into AWS CloudShell. It runs `apply.sh` from this directory
at a pinned commit, after checking its checksum:

1. installs Terraform and kubectl if missing, checked against pinned checksums.
   Everything big (Terraform, its providers, about 1 GB, and the checkout) goes
   under `/tmp/simple-host`: CloudShell keeps only 1 GB in your home directory.
   A copy of your answers goes to `~/simple-host/<address>.tfvars`;
2. fetches this repository at that commit and writes `terraform.tfvars`;
3. keeps the Terraform state in `s3://<name>-tfstate-<account>-<region>`
   (versioning, encryption, TLS only, lockfile locking);
4. on an existing cluster, uses a cert-manager (1.14 or later) or External
   Secrets Operator (serving `external-secrets.io/v1`) already there, and
   stops with the exact fix when one is too old or only its CRDs are left
   behind. It adds the cluster's IAM OIDC provider when it is missing, outside
   Terraform, so removing Simple Host never removes it;
5. asks once for the client secret (typed, not shown);
6. shows the plan and waits for `yes`, then works in two steps: the cluster and
   the database (the long part), then the rest.

**Keep the tab open.** A new cluster takes about 25 minutes, yours about 15.
AWS CloudShell ends a session after 20 to 30 minutes without a key press,
which can land in the middle. Closing the tab, Ctrl-C or a hang-up stops
Terraform cleanly: it saves what is done and releases the lock. Paste the same
line again, from any shell, and it picks up where it stopped:

- a run cut off with no chance to stop leaves its lock; every run keeps a
  heartbeat next to the state, and a lock with no heartbeat for 3 minutes is
  cleared (a run that is still going is left alone, and the line says so);
- a cluster, node group or database whose creation was cut short is waited
  for and kept (or adopted, if the state never heard of it), so the long parts
  are not made twice;
- a Helm install cut short is cleared and made again.

An AWS account on the free plan cannot launch the nodes or the database at
their default sizes; `apply.sh` says so before it starts. Upgrade the account's
plan first.

`--plan` shows the plan and changes nothing (it creates no state bucket);
`--yes` skips the confirmation; `--destroy` removes what it made (see below).
It runs on Linux (AWS CloudShell or any Linux shell signed in to AWS), not on
macOS.

At the end it prints what to do for DNS:

- **A new zone** (the usual case): NS records for your address. If the address
  is under a domain you already manage (`sites.example.com` under
  `example.com`), add them in that domain's DNS zone. If it is a domain of its
  own, set them as its name servers at the registrar.
- **A Route 53 zone for the address already in the account**: it is used as it
  is, and there is nothing to add.

Then:

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
cluster-admin. On an existing cluster, create its IAM OIDC provider first if it
has none (`eksctl utils associate-iam-oidc-provider --cluster <name> --approve`).
The client secret is needed on the first apply only; later runs keep the stored
one (change it with
`aws secretsmanager put-secret-value --secret-id <name>/oidc --secret-string file:///dev/stdin`,
then `kubectl -n simple-host rollout restart deploy/simple-host`).

## Variables

`terraform.tfvars` from the setup page holds `create_cluster`, `cluster_name`
(existing cluster), `region`, `base_domain`, `admin_emails`,
`allowed_email_domains` (with Google), `oidc_issuer`, `oidc_client_id` and,
from its Advanced step, `extra_config` (more `config.env` settings,
`docs/configuration.md`). Everything else has a default in `variables.tf`:

- `name` (the resource prefix, derived from the address),
- `image_digest` (the pinned release),
- `node_size` and `node_count` (used when the cluster is created; scale it
  later in EKS),
- `api_access_cidrs` (who may reach a new cluster's Kubernetes API over the
  internet; everywhere by default, because CloudShell has no fixed address;
  narrow it to your network once the install is done),
- `db_size`, `db_backup_days` (7),
- `protect_data` (deletion protection on the database and the bucket; `true`),
- `dns_zone_id`, `letsencrypt_email`, `tags`.

## Certificates and Let's Encrypt's limits

Every owner (a person or team who publishes) gets a `*.<owner>.<address>`
certificate from Let's Encrypt, the first time they publish. Let's Encrypt
issues at most **50 new certificates per registered domain per week** (shared
with anything else that uses Let's Encrypt on that domain), and the same
certificate at most 5 times a week. With more than about 50 people publishing
in the first week, the rest wait: their sites are served at
`<owner>.<address>/<site>/` until their certificate arrives (cert-manager
retries by itself, up to a day or two apart).

For a big rollout, issue owner certificates from your own CA instead: create a
cert-manager CA ClusterIssuer (`INSTALL.md`, "Site addresses"; the company's
devices must trust the CA) and add to `terraform.tfvars`:

```hcl
extra_config = {
  OWNER_CERT_ISSUER = "company-ca"
}
```

The address's own certificate stays with Let's Encrypt.

## Upgrades, changes, removal

- **Upgrade**: set `image_digest` to the new release's verified digest
  (`INSTALL.md` section 3) and run the line again (or `terraform apply`). The
  pods roll over one at a time; the namespace, the certificates and the owner
  Ingresses stay.
- **A setting**: add it to `extra_config` and apply. The pods roll over.
- **Remove**: `--destroy` refuses while `protect_data` is `true` (the
  default), so a slip cannot delete the database and every site. Take the data
  you need first (`docs/uninstall.md`), add `protect_data = false` to
  `terraform.tfvars`, run the line once as it is, then again with `--destroy`
  (it asks you to type the address). Delete the NS records at your DNS
  provider too.
- **What removal leaves on a cluster of yours**: the IAM OIDC provider, the
  empty `cert-manager`, `external-secrets` and `simple-host-ingress`
  namespaces, and the cert-manager and External Secrets CRDs (Helm keeps
  CRDs). A later install adopts them.
- **What the load balancer adds**: the Network Load Balancer the cluster makes
  for Traefik adds rules for its node ports to the nodes' security group; on a
  cluster of yours, that group is shared.

## Costs

The NAT gateway of a new cluster's VPC is about $33 a month plus $0.045 per GB
it carries (us-east-2 list prices). The rest (two `t3.medium` nodes, the EKS
control plane, `db.t4g.small`, the NLB, Route 53, Secrets Manager) is in
https://simple-host.app/costs.
