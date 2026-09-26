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

Serving therefore depends on the database being up: every page request
looks up the site's live version there, so a database outage takes the
sites down with it (they answer 503), not just deploys and sign-in.

Each pod keeps a local cache of the versions it serves in an `emptyDir` at
`CACHE_DIR`, emptied on start and bounded by `CACHE_MAX_BYTES` (versions
being served are pinned, so the volume is sized at about 3x that). A pod
holds nothing that is not in the bucket or the database, so the Deployment
runs two replicas, one per node when there are several, with rolling
updates and a PodDisruptionBudget. `/readyz` fails only on the database
and its schema. It also checks that the bucket can be read (it reads a key
that is never written and expects "not found"), but a bucket fault does not
take pods out of rotation: every pod shares the bucket, so it would take
them all out at once. While the bucket is failing, pages already in a pod's
cache keep serving; uncached pages answer 503 and publishing fails (500)
until it is fixed. Each failed check is logged as `readyz: bucket: ...` and
sets the `simplehost_bucket_ok` metric to 0: alert on it (checked at most
every 10 seconds, like the rest of the probe).

Each pod opens at most 20 database connections, and a rollout runs one
extra pod. Size Postgres `max_connections` for (replicas + 1) x 20, plus a
few for the migrate and prune jobs and your own sessions: at least 65 for
the default two replicas. A small managed plan can be lower than that
(UpCloud's 1 GB plan allows 50).

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
  abort incomplete multipart uploads after 7 days. With the AWS CLI (add
  `--endpoint-url https://<endpoint>` for any other S3-compatible
  provider):

```sh
aws s3api put-bucket-versioning --bucket simple-host-backups --versioning-configuration Status=Enabled
```

```sh
aws s3api put-bucket-lifecycle-configuration --bucket simple-host-backups --lifecycle-configuration '{"Rules":[{"ID":"simple-host","Status":"Enabled","Filter":{},"NoncurrentVersionExpiration":{"NoncurrentDays":30},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":7},"Expiration":{"ExpiredObjectDeleteMarker":true}}]}'
```

Without the AWS CLI, any S3 client that can sign a request sends the same
configuration as XML. With curl 7.75 or later, set the rule, then send
versioning and the rule (`<endpoint>`, `<bucket>`, `<region>` and the key
pair are the bucket's; the lifecycle call needs the `Content-MD5` header):

```sh
RULE='<LifecycleConfiguration><Rule><ID>simple-host</ID><Status>Enabled</Status><Filter/><NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays></NoncurrentVersionExpiration><AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule></LifecycleConfiguration>'
```

```sh
curl -fsS -X PUT --aws-sigv4 "aws:amz:<region>:s3" --user "<key-id>:<secret>" --data-binary '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>' "https://<endpoint>/<bucket>?versioning"
```

```sh
curl -fsS -X PUT --aws-sigv4 "aws:amz:<region>:s3" --user "<key-id>:<secret>" -H "Content-MD5: $(printf %s "$RULE" | openssl dgst -md5 -binary | base64)" --data-binary "$RULE" "https://<endpoint>/<bucket>?lifecycle"
```

Read both back (the same commands without `-X PUT`, the header and the
body) before you rely on them. Google Cloud Storage (object versioning plus
lifecycle rules), OCI (object versioning plus lifecycle policy) and MinIO
(`mc version enable`, `mc ilm rule add`) also have their own controls. The
local overlay's MinIO turns versioning on when it creates the bucket.

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
`BACKUP_SSE_KEY_ID`). `BACKUP_ENVELOPE_KEY` adds a client-side envelope, so
the bucket alone cannot read the objects. **Escrow the key in your
organisation's secret store before first use:** the bucket is the only copy
of every site, every object in it (and every noncurrent version) is
encrypted under the key, and losing the key loses every site.

Since v1.1.3 each enveloped object is also bound to its own object key
(the key is the encryption's associated data, marked by the
`sh-envelope-format: 2` metadata field): an object copied or moved to
another site's key does not decrypt, so someone who can write to the bucket
cannot swap one site's content in for another's. Objects written by v1.1.0
to v1.1.2 carry no format field and are still read (the server logs this
once per start); they stay in the older form until rewritten, and every new
deploy, upload and restore writes the bound form. Assets are also checked
against the SHA-256 their row recorded before they are served.

With the envelope on, an object without it is refused: only someone with
bucket access could have put it there. An install that added
`BACKUP_ENVELOPE_KEY` after it already held sites sets
`BACKUP_ENVELOPE_PLAINTEXT_ALLOWED=true` so those older, unenveloped objects
stay readable.

Rotating the envelope key: add the new key second, deploy, swap the order so
the new key wraps new objects, deploy. **Never remove the old key.** Objects
are immutable and long-lived; removing a key makes every object wrapped
under it unreadable.

## Migrating from a PVC install

Releases before v1.1.0 kept sites on a `simple-host-site-data` volume and
copied them to the bucket. Upgrading from v1.0.x to v1.1.0 or later requires
these steps; applying the new manifests alone starts pods that cannot find
any site in the bucket. Replace `<digest>` with the new release's digest
(`INSTALL.md` section 3).

1. Back up the database. Either take your provider's on-demand backup or
   snapshot and note its time (point-in-time recovery to a time before
   step 5 also works), or dump it from inside the cluster with the owning
   role. Start a pod that has the install's database settings:

   ```sh
   kubectl -n simple-host run simple-host-pgdump --restart=Never --image=postgres:16.11 --overrides='{"apiVersion":"v1","spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"simple-host-pgdump","image":"postgres:16.11","command":["sleep","3600"],"env":[{"name":"PGSSLMODE","value":"verify-full"},{"name":"PGSSLROOTCERT","value":"/ca/ca.crt"}],"envFrom":[{"configMapRef":{"name":"simple-host-config"}},{"secretRef":{"name":"simple-host-secrets"}}],"volumeMounts":[{"name":"db-ca","mountPath":"/ca","readOnly":true}],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}],"volumes":[{"name":"db-ca","secret":{"secretName":"simple-host-db-ca"}}]}}'
   ```

   Wait for it, dump, check the file starts with `PGDMP`, and remove the pod:

   ```sh
   kubectl -n simple-host wait --for=condition=Ready pod/simple-host-pgdump --timeout=120s
   ```

   ```sh
   kubectl -n simple-host exec simple-host-pgdump -- sh -c 'PGPASSWORD="$DB_PASSWORD" exec pg_dump -Fc -h "$DB_HOST" -p "${DB_PORT:-5432}" -U "$DB_USER" -d "$DB_NAME"' > simplehost-before-upgrade.dump
   ```

   ```sh
   head -c5 simplehost-before-upgrade.dump; echo; kubectl -n simple-host delete pod simple-host-pgdump
   ```

   Keep the file somewhere private: it holds every account and state
   document. `pg_restore --clean -d <database>` restores it.
2. Stop the old workloads:
   `kubectl -n simple-host scale deploy/simple-host --replicas=0 && kubectl -n simple-host delete cronjob simple-host-backup-assets --ignore-not-found`
3. Turn on bucket versioning and the lifecycle rule (above).
4. Dry run. Apply the first Job below. It reads the old volume and the
   database and changes neither: no migrations run, nothing is uploaded.
   Its log must end with `0 failed`:
   `kubectl -n simple-host logs job/simple-host-migrate-storage-dry-run`.
   If it reports failures, fix them, or undo step 2 (scale back to the old
   replica count and re-apply the old manifests) and stop here.
5. Apply the second Job. **This is the step that migrates the database**:
   its `migrate` init container applies the new release's schema
   migrations, which the old release cannot run against afterwards (only
   step 1's backup undoes them). Then `migrate-storage` uploads every
   site's retained versions and live assets, re-downloads each to verify
   it, and exits non-zero if anything failed or is missing. It is safe to
   re-run.
6. Check the log ends with zero failures:
   `kubectl -n simple-host logs job/simple-host-migrate-storage -c migrate-storage`
7. Apply the new release's manifests.
8. Verify: open a few sites, and run `make smoke BASE=...` if you use it.
9. Only then delete the old volume
   (`kubectl -n simple-host delete pvc simple-host-site-data`) and the old
   backup objects. Those are `<prefix><owner>/<site>/v<N>-<timestamp>.tar.gz`
   and `<prefix><owner>/<site>/assets/<id>`: everything under the prefix
   except `<prefix>sites/` (`sites` is a reserved name, so no owner uses it).
   They are no longer read. If the volume's reclaim policy is `Retain`
   (the base used it, and some providers' storage classes keep the disk
   anyway), also delete the released PersistentVolume and the disk in the
   provider's console, or it keeps being billed.

Step 4, the dry run:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: simple-host-migrate-storage-dry-run
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
      containers:
        - name: migrate-storage
          image: ghcr.io/vineetu/simple-host-enterprise@sha256:<digest>
          args: ["migrate-storage", "-from", "/mnt/data/sites", "-dry-run"]
          envFrom:
            - configMapRef: { name: simple-host-config }
            - secretRef: { name: simple-host-secrets }
          volumeMounts:
            - { name: site-data, mountPath: /mnt/data, readOnly: true }
            - { name: db-ca, mountPath: /etc/simple-host/db-ca, readOnly: true }
            - { name: tmp, mountPath: /tmp }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
      volumes:
        - name: site-data
          persistentVolumeClaim: { claimName: simple-host-site-data, readOnly: true }
        - name: db-ca
          secret: { secretName: simple-host-db-ca, optional: true }
        - name: tmp
          emptyDir: {}
```

Step 5, migrations and the real copy:

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
        # Applies the new release's database migrations.
        - name: migrate
          image: ghcr.io/vineetu/simple-host-enterprise@sha256:<digest>
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
          image: ghcr.io/vineetu/simple-host-enterprise@sha256:<digest>
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
