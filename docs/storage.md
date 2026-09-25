# Site storage

## How it works

The S3-compatible bucket named by `BACKUP_STORAGE_*` is the site store. The
variable names keep their old prefix; the bucket is no longer a backup
target. Under `BACKUP_STORAGE_PREFIX`:

```
sites/<site-id>/v<N>.tar.gz     one deployed version, written once, never changed
sites/<site-id>/assets/<id>     one uploaded asset, written once, never changed
```

Postgres decides what is served: `sites.active_version` names the live
version, and switching versions is a row update, not a file operation. The
state document, users, teams and audit log live only in Postgres.

Each pod keeps a local cache of the versions it serves in an `emptyDir` at
`CACHE_DIR`, emptied on start and bounded by `CACHE_MAX_BYTES` (versions
being served are pinned, so the volume is sized at about 3x that). A pod
holds nothing that is not in the bucket or the database, so the Deployment
runs two replicas with rolling updates and a PodDisruptionBudget. `/readyz`
checks the database and the bucket.

Each pod opens at most 20 database connections. Size Postgres
`max_connections` for replicas x 20, plus the migrate and prune jobs.

## Bucket requirements

- The S3 API. AWS S3, MinIO, Google Cloud Storage (XML API with HMAC keys)
  and OCI Object Storage (S3 Compatibility API) work; see `docs/cloud/`.
- On AWS S3, leave `BACKUP_STORAGE_ACCESS_KEY_ID`/`BACKUP_STORAGE_SECRET_ACCESS_KEY`
  unset to use IRSA or EKS Pod Identity through the SDK's default credential
  chain. `BACKUP_SSE=AES256` (SSE-S3) and `BACKUP_SSE=aws:kms` with
  `BACKUP_SSE_KEY_ID` (SSE-KMS) both work; the role needs `s3:GetObject`,
  `s3:PutObject`, `s3:DeleteObject`, `s3:ListBucket` on the bucket (plus
  `kms:GenerateDataKey`/`kms:Decrypt` on the key for SSE-KMS). The SDK's
  default request checksums stay on for Amazon S3; for every other endpoint
  they are sent only when an operation requires them, which GCS and OCI need.
- Azure Blob Storage has no S3 API. It needs an S3 front for now: the
  in-cluster MinIO component or an S3-to-Blob proxy such as `s3proxy`
  (`docs/cloud/aks.md` section 3).
- **Versioning on, with a lifecycle rule.** Required. Expire noncurrent
  versions after a retention window (30 days is a reasonable start) and
  abort incomplete multipart uploads after 7 days. On AWS:

```sh
aws s3api put-bucket-versioning --bucket simple-host-backups --versioning-configuration Status=Enabled
```

```sh
aws s3api put-bucket-lifecycle-configuration --bucket simple-host-backups --lifecycle-configuration '{"Rules":[{"ID":"simple-host","Status":"Enabled","Filter":{},"NoncurrentVersionExpiration":{"NoncurrentDays":30},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":7},"Expiration":{"ExpiredObjectDeleteMarker":true}}]}'
```

Google Cloud Storage (object versioning plus lifecycle rules), OCI (object
versioning plus lifecycle policy) and MinIO (`mc version enable`,
`mc ilm rule add`) have the same controls. The local overlay's MinIO turns
versioning on when it creates the bucket.

This is what replaces the old backup job. An object the server deletes or
overwrites stays recoverable as a noncurrent version for the retention
window. It covers sites and assets only: back up Postgres on its own
(point-in-time recovery on a managed database), since it is the source of
truth for what is live, the state documents, and the users.

## Retention

When a version is retired or a site is deleted, its objects are queued in
the database and deleted from the bucket by the server one hour after they
stop being referenced. After that only bucket versioning keeps them, until
the lifecycle rule expires them.

## Encryption

Every write carries the server-side-encryption header (`BACKUP_SSE`,
`BACKUP_SSE_KEY_ID`). `BACKUP_ENVELOPE_KEY` adds an optional client-side
envelope, so the bucket alone cannot read the objects.

Rotating the envelope key: add the new key second, deploy, swap the order so
the new key wraps new objects, deploy. **Never remove the old key.** Objects
are immutable and long-lived; removing a key makes every object wrapped
under it unreadable.

## Migrating from a PVC install

Earlier releases kept sites on a `simple-host-site-data` volume and copied
them to the bucket. To move an existing install:

1. Back up the database.
2. Stop the old workloads:
   `kubectl -n simple-host scale deploy/simple-host --replicas=0 && kubectl -n simple-host delete cronjob simple-host-backup-assets --ignore-not-found`
3. Turn on bucket versioning and the lifecycle rule (above).
4. Run the new release's `migrate` and `migrate-storage` in a one-off Job
   that mounts the old volume read-only (below; set `image` to the new
   release). `migrate-storage` uploads every site's retained versions and
   live assets, re-downloads each to verify it, and exits non-zero if
   anything failed or is missing. It is safe to re-run.
5. Check the log ends with zero failures:
   `kubectl -n simple-host logs job/simple-host-migrate-storage -c migrate-storage`
6. Apply the new release's manifests.
7. Verify: open a few sites, and run `make smoke BASE=...` if you use it.
8. Only then delete the old volume
   (`kubectl -n simple-host delete pvc simple-host-site-data`) and the old
   backup objects. Those are `<prefix><owner>/<site>/v<N>-<timestamp>.tar.gz`
   and `<prefix><owner>/<site>/assets/<id>`: everything under the prefix
   except `<prefix>sites/` (`sites` is a reserved name, so no owner uses it).
   They are no longer read.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: simple-host-migrate-storage
  namespace: simple-host
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: simple-host
      automountServiceAccountToken: false
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile: { type: RuntimeDefault }
      initContainers:
        - name: migrate
          image: ghcr.io/example/simple-host@sha256:NEW_RELEASE_DIGEST
          args: ["migrate"]
          envFrom: &env
            - configMapRef: { name: simple-host-config }
            - secretRef: { name: simple-host-secrets }
          env:
            - name: DB_APP_PASSWORD
              valueFrom: { secretKeyRef: { name: simple-host-secrets, key: DB_APP_PASSWORD } }
          volumeMounts:
            - { name: db-ca, mountPath: /etc/simple-host/db-ca, readOnly: true }
          securityContext: &sc
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
      containers:
        - name: migrate-storage
          image: ghcr.io/example/simple-host@sha256:NEW_RELEASE_DIGEST
          args: ["migrate-storage", "-from", "/mnt/data/sites"]
          envFrom: *env
          volumeMounts:
            - { name: site-data, mountPath: /mnt/data, readOnly: true }
            - { name: db-ca, mountPath: /etc/simple-host/db-ca, readOnly: true }
            - { name: tmp, mountPath: /tmp }
          securityContext: *sc
      volumes:
        - name: site-data
          persistentVolumeClaim: { claimName: simple-host-site-data, readOnly: true }
        - name: db-ca
          secret: { secretName: simple-host-db-ca, optional: true }
        - name: tmp
          emptyDir: {}
```

Add `-dry-run` to the args to see what would be uploaded without writing.

## Restoring a version

`restore` copies a stored version of any site, live or deleted, into a
target site as its next version, creating the target site if needed:

```sh
kubectl -n simple-host exec deploy/simple-host -- /simple-host restore -from-site-id <uuid> -version <n> -owner <username> -site <name> -set-current=true
```

`-set-current=false` adds the version without making it live. A deleted
site's id is in the audit log (its `site_delete` event). If the object has
already been swept, first bring back its noncurrent version with the
bucket's own tools, for example on AWS: find it with
`aws s3api list-object-versions --bucket <bucket> --prefix <prefix>sites/<uuid>/`,
copy it back with
`aws s3api copy-object --bucket <bucket> --key <key> --copy-source "<bucket>/<key>?versionId=<id>" --server-side-encryption AES256`,
then run `restore`. Assets are not restored.
