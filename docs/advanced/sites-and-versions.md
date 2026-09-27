# Sites and versions

Every publish is a new version; the owner can publish without going live, open a one-hour
preview link, roll back, rename, move a site into a team and download it as one zip. Pages can
upload files (images, video, PDFs) within per-site limits.

**Quotas** apply per person or team: sites, and bytes across every kept version and uploaded
file. A deploy that frees as much as it adds is always accepted, so an owner over a lowered
limit can still update.

**Malware scan.** With `CLAMD_ADDR` set, every file of a deploy and every uploaded file is
scanned before it is stored; a scanner that is down refuses uploads rather than letting them
through. Without it, installers and scripts are still refused by file type.

**Size.** Raise the ingress body-size limit (`ingress-patch.yaml`) with `MAX_ARCHIVE_BYTES`, and
the pod's memory with `UPLOAD_CONCURRENCY`: each upload holds its archive and files in memory.

<!-- settings:group=sites -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `MAX_ARCHIVE_BYTES` | `104857600` | 1048576–524288000 bytes | The largest archive one deploy may send. Raise the ingress body-size limit with it. |
| `MAX_FILES_PER_SITE` | `50000` | 1–100000 files | Files one deploy may hold. |
| `UPLOAD_CONCURRENCY` | `2` | 1–64 uploads | Uploads (and, separately, downloads) one replica processes at once. Raise the pod's memory with it. |
| `QUOTA_MAX_SITES` | `1000` | at least 0 sites | Sites one person or team may have. 0 is unlimited. |
| `QUOTA_MAX_BYTES` | `10737418240` | at least 0 bytes | Storage one person or team may use, every kept version and uploaded file included. 0 is unlimited. |
| `QUOTA_MAX_VERSIONS` | `5` | 1–100 versions | Versions kept per site; older ones are removed after each deploy. |
| `PREVIEW_LINK_TTL` | `1h` | 1m–24h Go duration | How long a preview link to a kept version works. **Security-sensitive.** |
| `EXPORT_LINK_TTL` | `10m` | 1m–24h Go duration | How long a whole-site download link works (once). **Security-sensitive.** |
| `CLAMD_ADDR` | none | text | A clamd scanner (host:port) that checks every uploaded file before it is stored. A scanner that is down refuses uploads. **Security-sensitive.** |
| `CLAMD_TIMEOUT` | `30s` | Go duration | How long the scan of one file may take. |
| `ASSET_MAX_FILE_BYTES` | `26214400` | at least 1 bytes | The largest file a page may upload. |
| `ASSET_MAX_SITE_BYTES` | `524288000` | at least 1 bytes | Uploaded files one site may hold, in bytes. |
| `ASSET_MAX_SITE_COUNT` | `5000` | at least 1 files | Uploaded files one site may hold. |
| `SEARCH_SESSION_MAX_AGE` | `4320h` | 1h–9600h Go duration | Lifetime of the anonymous cookie site search uses for its limits and counts. |
| `RATE_LIMIT_MANAGEMENT_CLIENT` | `60/1s` | any (warns past 10× looser) | Management API calls per client address. |
| `RATE_LIMIT_MANAGEMENT_USER` | `30/10s` | any (warns past 10× looser) | Management changes per person. |
| `RATE_LIMIT_SEARCH_QUERY_PEER` | `200/50ms` | any (warns past 10× looser) | Site searches per network peer. |
| `RATE_LIMIT_SEARCH_QUERY_SESSION` | `60/1s` | any (warns past 10× looser) | Site searches per search session. |
<!-- /settings -->

## Recipes

**Bigger sites.** `MAX_ARCHIVE_BYTES=262144000` (250 MiB), the ingress
`nginx.ingress.kubernetes.io/proxy-body-size: 250m`, and more pod memory.

**Scan every upload.** Run clamd as a sidecar and set `CLAMD_ADDR=127.0.0.1:3310`.
