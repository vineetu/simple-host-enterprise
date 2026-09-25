# Per-cloud install templates

Every file here fills the same seven sections, in this order. The section says
what must be true; each cloud file says how on that cloud.

| Cloud | File |
|---|---|
| AWS (EKS) | [aws.md](aws.md) |
| Google Cloud (GKE) | [gcp.md](gcp.md) |
| Azure (AKS) | [azure.md](azure.md) |
| Oracle Cloud (OKE) | [oci.md](oci.md) |
| UpCloud (UKS) | [upcloud.md](upcloud.md) |

Values go in `deploy/overlays/byo/config.env` / `secrets.env`; the ingress block
goes in `deploy/overlays/byo/ingress-patch.yaml`.

## The sections

1. **Cluster & ingress.** One ingress in front of the `simple-host` Service
   serving the base host and `*.<base>`, TLS terminated there, HTTP redirected to
   HTTPS, request body up to 128 MiB, idle/backend timeout 300 s, health check on
   `/healthz`.
2. **DNS & wildcard certificate.** `<base>` and `*.<base>` resolve to the ingress;
   one certificate covers both names. A wildcard needs DNS-01 (or a cloud-managed
   certificate that supports wildcards).
3. **Postgres.** Managed Postgres 16, TLS enforced on the server, the app connecting
   with `DB_SSLMODE=verify-full` and the provider's CA bundle in
   `deploy/overlays/byo/db-ca.crt`; `DB_HOST` is a name the server certificate
   covers. Point-in-time recovery on, 7+ days. Nothing in the package backs up
   Postgres; PITR is the database backup. Extension used by the migrations: `pgcrypto`.
4. **Bucket.** An S3-compatible bucket over HTTPS with versioning on (versioning is
   the file backup) and a lifecycle rule that expires noncurrent versions after your
   retention window. Encryption at rest on.
5. **Identity for pods.** The pod reaches the bucket with a workload identity where
   the server can use one, otherwise with a static key pair in `secrets.env`.
6. **OIDC provider notes.** The issuer is reachable from inside the cluster (an
   unreachable issuer makes the pod crash-loop), and the app registration admits
   only your organisation.
7. **Verify.** Rollout finished, `/healthz` answers over public HTTPS, the issuer
   answers from inside the cluster, `make smoke` passes.

## Site data

The bucket is the primary site store. Pods are stateless: no PersistentVolumeClaim
and no StorageClass for site data, two replicas, rolling updates.
