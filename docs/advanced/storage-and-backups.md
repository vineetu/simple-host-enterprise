# Storage and backups

- **Postgres** holds accounts, keys, sites, saved data and the audit log. Use a managed Postgres
  with point-in-time recovery; nothing in this package backs it up. The server connects as a
  least-privilege role (`DB_APP_USER`), migrations as the owning role, over verified TLS.
- **An S3-compatible bucket** holds every site version and uploaded file, with server-side
  encryption and, optionally, a client-side envelope key. Turn on bucket versioning: it is what
  recovery comes from.
- **Pods are stateless** and serve through a local cache (`CACHE_DIR`), emptied on start.

<!-- settings:group=storage -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `DB_HOST` | none | text | Your managed Postgres host. Or give DB_DSN instead of the parts. **Required.** |
| `DB_PORT` | `5432` | 1–65535 | Its port. |
| `DB_NAME` | none | text | The database name. **Required.** |
| `DB_USER` | none | text | The owning role the migrate and prune jobs connect as. **Required.** |
| `DB_PASSWORD` | none | secret | The owning role's password. Never the same as DB_APP_PASSWORD. **Required.** **Security-sensitive.** |
| `DB_APP_USER` | none | text | The least-privilege role the server connects as (the manifest sets simplehost_app). |
| `DB_APP_PASSWORD` | none | secret | That role's password; migrate sets it on every run. **Required.** **Security-sensitive.** |
| `DB_DSN` | none | secret | A complete postgres:// URL, instead of the parts above. **Security-sensitive.** |
| `DB_SSLMODE` | `verify-full` | text | TLS to the database. Anything but verify-full needs DB_INSECURE_ALLOWED. **Security-sensitive.** |
| `DB_SSL_ROOT_CERT` | none | text | The database's CA bundle, mounted from the simple-host-db-ca Secret. **Security-sensitive.** |
| `DB_INSECURE_ALLOWED` | `false` | `true` / `false` | Allows a database without verified TLS. Local evaluation only. **Security-sensitive.** |
| `DB_INCLUSTER_EVALUATION` | `false` | `true` / `false` | Set by the in-cluster evaluation Postgres, which nothing backs up; logs a warning at every start. Never set it on a real install: it marks the database as throwaway. **Security-sensitive.** |
| `BACKUP_STORAGE_ENDPOINT` | none | text | The S3-compatible bucket's endpoint, over HTTPS. **Required.** |
| `BACKUP_STORAGE_REGION` | `us-east-1` | text | The bucket's region, where the provider needs one. |
| `BACKUP_STORAGE_BUCKET` | none | text | The bucket that stores every site. Turn its versioning on. **Required.** |
| `BACKUP_STORAGE_PREFIX` | `backups/` | text | The folder inside the bucket. |
| `BACKUP_STORAGE_ACCESS_KEY_ID` | none | secret | The bucket's access key. Leave both keys out when a workload identity supplies credentials. **Security-sensitive.** |
| `BACKUP_STORAGE_SECRET_ACCESS_KEY` | none | secret | The bucket's secret key. **Security-sensitive.** |
| `BACKUP_STORAGE_INSECURE_ALLOWED` | `false` | `true` / `false` | Allows a plain-HTTP bucket endpoint. Local evaluation only. **Security-sensitive.** |
| `BACKUP_SSE` | `AES256` | `AES256` / `aws:kms` | Server-side encryption on every stored object. **Security-sensitive.** |
| `BACKUP_SSE_KEY_ID` | none | text | The KMS key, with BACKUP_SSE=aws:kms. **Security-sensitive.** |
| `BACKUP_ENVELOPE_KEY` | none | secret | Optional encryption of every object before it leaves the pod: <id>:<base64 32-byte key> entries. Escrow it: sites are unreadable without it. **Security-sensitive.** |
| `BACKUP_ENVELOPE_PLAINTEXT_ALLOWED` | `false` | `true` / `false` | Keeps reading objects written before the envelope key was added, until reencrypt has covered them. **Security-sensitive.** |
| `CACHE_DIR` | `/var/cache/simple-host` | text | The pod-local cache of site versions, emptied on start. |
| `CACHE_MAX_BYTES` | `1073741824` | at least 1 bytes | Size of that cache. Size its volume at about three times this. |
<!-- /settings -->

## Recipes

**Provider endpoints** ([../cloud/](../cloud/) has the full checklists):

| Provider | `BACKUP_STORAGE_ENDPOINT` |
|---|---|
| AWS S3 | `https://s3.<region>.amazonaws.com` |
| Google Cloud Storage | `https://storage.googleapis.com` |
| Oracle Cloud | `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com` |
| UpCloud | `https://<your endpoint>.upcloudobjects.com` |
| Azure | an S3-compatible front for Blob storage |

**Workload identity instead of keys.** Leave `BACKUP_STORAGE_ACCESS_KEY_ID` and
`BACKUP_STORAGE_SECRET_ACCESS_KEY` out of `secrets.env`.

**Envelope encryption.** `BACKUP_ENVELOPE_KEY=k1:$(openssl rand -base64 32)` in `secrets.env`,
escrowed in your secret store before first use. A restore drill (`simple-host restore`,
`simple-host verify-storage`) is in [../install.md](../install.md), section 9.
