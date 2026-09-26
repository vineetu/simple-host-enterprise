# Oracle Cloud (OKE)

Not yet run end to end on this cloud.

Fills the sections in [README.md](README.md).

## 1. Cluster & ingress

Pick one:

- **OCI native ingress controller** (OKE add-on). It provisions an OCI Load Balancer from
  the Ingress. Set `spec.ingressClassName` to the IngressClass you create for it, and
  delete the ingress-nginx lines. Raise the backend timeout to 300 s and allow 128 MiB
  bodies with its own settings. That setting is (unverified).
- **A Service of type LoadBalancer** in front of an in-cluster ingress controller, with
  flexible-shape annotations on that Service:

  ```yaml
  service.beta.kubernetes.io/oci-load-balancer-shape: flexible
  service.beta.kubernetes.io/oci-load-balancer-shape-flex-min: "10"
  service.beta.kubernetes.io/oci-load-balancer-shape-flex-max: "100"
  ```

  ingress-nginx is retired upstream (maintenance ended March 2026).

Security lists / NSGs:

| From | To | Port |
|---|---|---|
| Clients | LB subnet | 443, 80 |
| LB subnet | Worker nodes (NodePorts) or pod IPs (native pod networking) | Backend ports |
| Workers / pods | Object Storage, OIDC issuer | 443 egress |
| Workers / pods | DB subnet | 5432 |

## 2. DNS & wildcard certificate

Create `A` records for `<base>` and `*.<base>` pointing at the load balancer IP, in OCI DNS or
wherever the zone lives.

cert-manager has no built-in OCI DNS solver. Pick one:

- Host the zone with a provider cert-manager supports.
- Run a community OCI DNS webhook.
- Issue the wildcard out of band into the `simple-host-tls` Secret.

## 3. Postgres

OCI Database with PostgreSQL 16. It only accepts TLS.

- Extensions: `pgcrypto` is available by default, so no `oci.admin_enabled_extensions`
  configuration is needed for it.
- CA: download it from the DB system's Connection details in the console, or with
  `oci psql connection-details get --db-system-id <ocid>`. The CA field in that output
  is (unverified). Save it as `deploy/overlays/byo/db-ca.crt`.
- PITR: backups on, with 7+ days retention in the backup policy.
- Encryption at rest: OCI encrypts the database storage and backups by default
  (Oracle-managed key; a Vault key can be chosen at creation).

```
DB_HOST=<the FQDN shown under Connection details>
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

Use the FQDN, not the private IP. `verify-full` checks the name.

## 4. Bucket

Object Storage through the S3 Compatibility API. Objects are always encrypted at rest.

```
oci os bucket create --compartment-id <ocid> --name <bucket> --versioning Enabled
oci os object-lifecycle-policy put --bucket-name <bucket> --items '[{"name":"expire-noncurrent","action":"DELETE","timeAmount":30,"timeUnit":"DAYS","isEnabled":true,"target":"previous-object-versions"}]'
```

```
BACKUP_STORAGE_ENDPOINT=https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
BACKUP_STORAGE_REGION=<region>
BACKUP_STORAGE_BUCKET=<bucket>
```

- The namespace is not the tenancy name. Get it with `oci os ns get --query data --raw-output`.
- Region is checked here, so set it.
- For a customer-managed key, assign a Vault key to the bucket. `BACKUP_SSE=aws:kms` does
  nothing useful on OCI.

The server sets request checksums for OCI compatibility.

## 5. Identity for pods

OKE workload identity exists on enhanced clusters, but the server's S3 client uses the
AWS SDK chain, so it uses static keys. Create a **Customer Secret Key**
(`oci iam customer-secret-key create --user-id <ocid> --display-name simple-host`) for a
user whose group can manage objects in that bucket only. Put the pair in
`BACKUP_STORAGE_ACCESS_KEY_ID` / `BACKUP_STORAGE_SECRET_ACCESS_KEY`. The secret is shown once.

## 6. OIDC provider notes

- Worker egress to the issuer on 443 (NAT gateway for private workers).
- **Okta:** set client authentication to "Client secret" with `client_secret_post`.
- **Entra ID:** single-tenant app, issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`.

## 7. Verify

```
kubectl -n simple-host rollout status deploy/simple-host
curl -fsS https://<base>/healthz
kubectl -n simple-host run oidc-check --rm -i --restart=Never --image=curlimages/curl -- curl -fsS https://<issuer>/.well-known/openid-configuration
make smoke BASE=https://<base> KEY_FILE=<path-to-admin-api-key>
```
