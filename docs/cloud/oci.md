# Oracle Cloud (OKE)

Same seven pieces as `docs/cloud/aws.md`; nothing here is compiled into the
application. Every value below is a Kubernetes manifest field or a value in
`deploy/overlays/byo/config.env`/`secrets.env`.

Oracle is the easiest of the four on storage, because Object Storage speaks
the S3 API natively — no proxy, no extra component, unlike Azure.

**Not yet run against a live tenancy.** Every command and field below comes
from Oracle's own reference rather than from an install someone did. Treat it
as the shape of the work, and correct it here the first time you do one.

## 1. Storage class with encryption

The block-volume CSI driver is installed on OKE by default. Block Volumes are
encrypted at rest with Oracle-managed keys always; name a Vault key only when
a reviewer requires a customer-managed one.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: simple-host-encrypted
provisioner: blockvolume.csi.oraclecloud.com
parameters:
  # Balanced is the default performance tier; "10" is the VPU value for it.
  vpusPerGB: "10"
  # Customer-managed key, only if required:
  # kmsKeyId: ocid1.key.oc1..<ocid>
allowVolumeExpansion: true
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

`reclaimPolicy: Retain` matters for any data volume you run in the cluster
(the application itself keeps sites in the bucket): a deleted PVC should not
take it.

## 2. Managed Postgres with TLS and PITR

OCI Database with PostgreSQL gives you automatic backups and point-in-time
recovery. TLS is on and not optional, which is what this package wants —
`DB_SSLMODE=verify-full` with the CA bundle mounted, and no
`DB_INSECURE_ALLOWED` anywhere near a real install.

Put the CA in a Secret and mount it where `DB_SSL_ROOT_CERT` points. The
database's own certificate chain is published by Oracle; fetch it rather than
copying one out of a blog post.

```
DB_HOST=<endpoint>.postgresql.<region>.oci.oraclecloud.com
DB_PORT=5432
DB_NAME=simplehost
DB_SSLMODE=verify-full
DB_SSL_ROOT_CERT=/etc/simple-host/db-ca/ca.crt
```

The state document lives only in Postgres, so this PITR window is its whole
backup story.

## 3. Object storage with default encryption

Object Storage is encrypted at rest always, with an Oracle-managed key unless
you assign one from Vault. It exposes an **S3 Compatibility API**, which is
what the storage client speaks. Turn on object versioning and a lifecycle
policy for previous versions (`docs/storage.md`).

Two things catch people:

- **The endpoint contains your tenancy's object-storage namespace**, which is
  not your tenancy name. Read it once with
  `oci os ns get --query data --raw-output`.
- **Credentials are a Customer Secret Key**, not your API signing key and not
  an auth token. Console → your user → Customer Secret Keys → Generate. The
  secret is shown once. These are the access-key/secret pair the S3 API wants;
  the RSA signing key the `oci` CLI uses is a different credential for a
  different API.

```
BACKUP_STORAGE_ENDPOINT=https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
BACKUP_STORAGE_REGION=<region>            # Oracle does check this one
BACKUP_STORAGE_BUCKET=simple-host-backups
BACKUP_STORAGE_PREFIX=backups/
BACKUP_SSE=AES256
```

Region is one of the places Oracle differs from the stores that ignore it, so
set it rather than leaving the default.

`BACKUP_SSE=AES256` asks for server-side encryption on the object. Oracle
encrypts every object regardless; confirm the header is accepted rather than
rejected the first time a site is deployed, and that the site then serves,
before you rely on it.

For a customer-managed key, assign a Vault key to the bucket at creation.
`BACKUP_SSE_KEY_ID` and `BACKUP_SSE=aws:kms` are an AWS-specific header pair
and will not do anything useful here — use the bucket's own key assignment.

## 4. Ingress / load balancer

OKE's native ingress controller provisions an OCI Load Balancer. Whichever
controller you choose, set the request body cap: uploads are archives up to
100 MiB and a proxy left at its default refuses them before the application
is reached. See `deploy/overlays/byo/ingress-patch.yaml`.

Running ingress-nginx on OKE is the better-trodden path, and it makes the
annotations in that file correct as written. Annotate its Service for the
load balancer shape you want:

```yaml
service.beta.kubernetes.io/oci-load-balancer-shape: flexible
service.beta.kubernetes.io/oci-load-balancer-shape-flex-min: "10"
service.beta.kubernetes.io/oci-load-balancer-shape-flex-max: "100"
```

## 5. DNS wildcard

Every owner is one label beneath the base host and a restricted site gets its
own label, so this needs a wildcard:

```
simple-host.example.com        A     <load balancer IP>
*.simple-host.example.com      A     <load balancer IP>
```

OCI DNS holds the zone if the domain is delegated to Oracle; otherwise it is
wherever the domain already lives. Nothing about the package cares which.

## 6. Certificate via cert-manager, DNS-01

A wildcard certificate cannot use HTTP-01. Use DNS-01 with whichever provider
holds the zone. cert-manager has no built-in OCI DNS solver, so either:

- delegate `simple-host.example.com` to a zone cert-manager does support, or
- run the community OCI webhook solver, or
- issue the wildcard out of band and put it in the Secret the Ingress names.

The third is the least moving parts and the most human toil. Pick knowingly.

## 7. The grant note a reviewer asks about

Same as every other platform: the application connects as `simplehost_app`,
which cannot alter the schema, and migrations run as the owning role from the
init container. Nothing OCI-specific — the database is just Postgres. See
`docs/install.md`.
