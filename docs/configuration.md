# Configuration reference

Every variable `internal/config/config.go` reads.
Everything under "Public address" through "Retention" is read by
`config.Load()` (the server) or `config.LoadDatabase()` /
`config.LoadAppRolePassword()` / `config.LoadAuditRetention()` (the
`migrate` and `prune` subcommands, which each need only a narrower slice of
the same environment). Nothing that identifies a particular installation
has a default: leave any of the required rows below unset and the process
refuses to start, reporting every missing name at once in a single
`missing required configuration: ...` error, not one restart per gap.


## Secrets: environment or file

Every secret below can be given directly, or through a file: set
`<NAME>_FILE` to a path and the value is read from there instead, with a
trailing newline trimmed. The file wins when both are set. A `_FILE` that
cannot be read stops startup with its own error rather than being reported as
a missing value — a broken mount and an unset variable need different fixes.
That includes `BACKUP_ENVELOPE_KEY_FILE`: an unreadable envelope key stops
startup rather than silently leaving backups without their envelope.

Applies to `OIDC_CLIENT_SECRET`, `SESSION_SIGNING_KEY`, `DB_PASSWORD`,
`DB_APP_PASSWORD`, `BACKUP_STORAGE_ACCESS_KEY_ID`,
`BACKUP_STORAGE_SECRET_ACCESS_KEY` and `BACKUP_ENVELOPE_KEY`.

**This is the cloud-neutral way to get a credential into the pod.** Each
cloud has its own workload-identity flow and they agree on nothing — the AWS
SDK's credential chain reads IRSA and EKS Pod Identity and has no idea what a
GKE or AKS workload identity is. What every cloud does share is a
Kubernetes-native projection from its own secret store: the Secrets Store CSI
driver, or External Secrets Operator writing a Secret. Both land a file in the
container, and both work the same way on every platform.

A file is also the safer of the two regardless of cloud. An environment
variable is visible to every child process, appears in crash dumps, and is
printed by anything that dumps its own environment; a file is readable only by
what opens it, and rotating it does not need the value to pass through a
shell.

## Public address

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `PUBLIC_BASE_URL` | Yes | none | Must be an absolute URL, origin only (no path beyond `/`, no query, no fragment, no userinfo). In `SECURE_MODE=true`, must be `https`. |
| `SECURE_MODE` | No | `false` | Must parse as a bool. When `true`, requires `PUBLIC_BASE_URL` to be `https` and `HTTPS_REDIRECT_PORT` to differ from `PORT`. |
| `PORT` | No | `8080` | none |
| `HTTPS_REDIRECT_PORT` | No | `8081` | Must differ from `PORT` when `SECURE_MODE=true`. |
| `CACHE_DIR` | No | `/var/cache/simple-host` | none. Pod-local cache of site versions, emptied on start; the Deployment mounts an `emptyDir` here (docs/storage.md). |
| `CACHE_MAX_BYTES` | No | `1073741824` (1 GiB) | Must parse as a positive integer. Bounds unpinned cache entries; versions being served are pinned and can exceed it, so size the volume at about 3x. |
| `METRICS_PORT` | No | `9090` | Port of the separate `/metrics` listener; not exposed by the Service or Ingress (`docs/install.md` section 12). |
| `TRUSTED_PROXY_CIDRS` | No | private ranges: `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7` | Comma-separated CIDRs (a bare address counts as one host) of the proxies in front of the server — typically the ingress controller's pod range. When a request's TCP peer is inside this set, the client address is taken from `X-Forwarded-For`, read right to left, as the first address that is not itself a trusted proxy; anything further left was written by the client and is ignored. The default covers an ingress controller or load balancer on a private address; set it explicitly (an empty value trusts nothing) if your proxies sit elsewhere, or if clients can reach the pod directly from a private network. That client address is the one used everywhere: rate limits, the request log, `access_log.ip`, `sessions.ip` and audit rows. A malformed entry is refused at startup. Rate limits on a route that has already signed the caller in are keyed by the person, not the address. |
| `RESERVED_LABELS` | No | none (empty) | Comma-separated; extends the built-in reserved-label set (`www`, `api`, `admin`, `sites`, `mcp`, `docs`, `auth`, `login`, `mail`, `cdn`, `status`, `app`, and the rest — `internal/handler/names.go`) with installation-specific hostnames that must never belong to an account or a site. Checked at account and site creation, and any label containing `--` anywhere is refused outright, independent of this list, since that shape is reserved for a restricted site's own hostname (`<owner>--<site>.<base>`, design 5.2a). |

## Identity (OIDC)

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `OIDC_ISSUER` | Yes | none | Discovery and JWKS are fetched from this issuer at startup (15-second timeout); if it is unreachable or its metadata does not match, the server exits and the pod crash-loops (it does not start and retry). Entra ID's multi-tenant endpoints (`/common`, `/organizations`, `/consumers`) are refused at startup: use the tenant's own issuer, `https://login.microsoftonline.com/<tenant id>/v2.0`. The issuer and the discovered authorization, token and JWKS endpoints must be `https://`; `http` is refused unless `OIDC_INSECURE_ALLOWED=true`. |
| `OIDC_INSECURE_ALLOWED` | No | `false` | Permits a plain-`http` issuer and discovered endpoints. For a local evaluation cluster only (the local overlay's in-cluster Dex sets it) — never set on a real install: the client secret and the signing keys would cross the network unprotected. |
| `OIDC_CLIENT_ID` | Yes | none | none at startup; the provider refuses the authorization request if wrong. |
| `OIDC_CLIENT_SECRET` | Yes | none | none at startup; token exchange fails if wrong. Belongs in a Secret, never in `config.env`. |
| `OIDC_SCOPES` | No | `openid email profile` | Space-separated. |
| `OIDC_EMAIL_CLAIM` | No | `email` | Overrides the ID token claim read as the person's address, for a provider that does not use `email`. Whatever the claim, sign-in requires the provider to vouch for the address: `email_verified` must be present and `true`, or it is refused (`sign_in_failed`, reason `email_not_verified`). Entra ID never sends `email_verified`; there, add the optional claim `xms_edov` to the app registration's ID token (Token configuration → Add optional claim), which Entra sets when the address's domain is verified by the tenant — it is accepted only from an Entra issuer. |
| `OIDC_USERNAME_CLAIM` | No | none (derives from the email's local part) | When set, this claim's value is used to derive the account's username instead. |
| `OIDC_ADMIN_CLAIM` | No | none | Must be set together with `OIDC_ADMIN_VALUE` — setting exactly one of the pair is refused at startup. |
| `OIDC_ADMIN_VALUE` | No | none | See `OIDC_ADMIN_CLAIM`. |
| `ADMIN_EMAILS` | No | none (empty) | Comma-separated, lowercased. A person is admin if their address is in this list, or `OIDC_ADMIN_CLAIM`/`OIDC_ADMIN_VALUE` matches (either source grants it), refreshed on every sign-in. When `OIDC_ADMIN_CLAIM` is not set, this list is also re-applied to every account at server start, so removing someone demotes them on the next deploy rather than at their next sign-in (with the claim in use, the claim can only be read at sign-in, so there the bound is `SESSION_TTL`). The portable admin path every reference install documents — Google does not put groups in the ID token. |
| `ALLOWED_EMAIL_DOMAINS` | No | none (empty, meaning unrestricted) | Comma-separated, lowercased. Required in practice for Google: without it, anyone with a Google account can sign in. With `OIDC_ISSUER=https://accounts.google.com` and this list set, the ID token must also carry `hd` (a Google Workspace account) naming one of these domains — a consumer Google account can hold a verified address at your domain without your company controlling it. Also gates whether an existing row may be claimed by a new sign-in's (verified) email — without this list, that claim path never runs. |
| `OIDC_HINT_DOMAIN` | No | the sole `ALLOWED_EMAIL_DOMAINS` entry, if there is exactly one | Sent as the provider's domain hint (Google: `hd`) on the authorization request. A hint narrows the account chooser; it never authorizes — the callback still checks the claim and the domain list independently. |
| `OAUTH_REDIRECT_HOSTS` | No | `chatgpt.com,claude.ai,vscode.dev,localhost,cursor://anysphere.cursor-mcp` | Where an AI app connecting to `/mcp` over OAuth may be sent back after sign-in. Comma-separated: a hostname allows `https` redirects to it, `localhost` allows loopback redirects for apps on the person's own machine (Claude Code, Codex), `scheme://host` allows an app's own URL scheme, and `*` allows any `https` host. Apps register themselves; this list is what limits which ones can finish connecting. |

## Sessions

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `SESSION_SIGNING_KEY` | Yes | none | One or two comma-separated `<id>:<base64 32-byte key>` entries. More than two, a duplicate id, a non-base64 value, or a decoded length other than 32 bytes is refused. The `__Host-` session cookie has no insecure fallback, so this is required even on a rehearsal install. Rotation: add the new key second, deploy, swap the order so it signs, deploy, remove the old key after `SESSION_TTL` has fully elapsed. |
| `SESSION_TTL` | No | `8h` | How long a sign-in lasts, however active. At most `24h`; a positive Go duration. Refused at startup when out of range, so a typo cannot make sessions effectively permanent. Default: a working day, which is also where common IdPs set their own session default. Owner-host and restricted-site hand-off sessions share the sign-in's row and expiry, so none outlives it. |
| `SESSION_IDLE` | No | `30m` | How long a sign-in survives unused. At most `8h` and never longer than `SESSION_TTL`; refused at startup otherwise. Default: ends an unattended browser's session well inside the working day. |
| `OAUTH_ACCESS_TTL` | No | `1h` | Lifetime of an access token an AI app holds for `/mcp` (the OAuth connector). At most `24h`, never longer than `OAUTH_REFRESH_TTL`. Short, because a copy of the token held elsewhere stays usable until it expires even after the app is revoked. |
| `OAUTH_REFRESH_TTL` | No | `720h` (30 days) | How long an AI app stays connected before the person must sign in at the IdP again. At most `2160h` (90 days). Measured from the sign-in that connected the app, not from the last refresh: rotating the refresh token never extends it. Go durations have no day unit; write days as hours. |
| `API_KEY_MAX_DAYS` | No | `365` | The longest lifetime an API key may be minted with; must be 1 to 365. API keys are for CI and other automation (people and their agents sign in through OIDC): a new key lives 90 days unless the mint request names `expires_in_days` (or the maximum, if it is below 90), an expired key is refused like a revoked one, and every new key starts with `shk_` so secret scanners can find it. Each key carries a scope chosen when it is minted: `publish` (the default: deploy, update, roll back and list sites, their versions, archives, saved data and assets, `/api/me` and `/mcp`), `full` (everything the person can do through the REST API except administration), or `offboard` (admins only: `POST /api/admin/users/disable` and nothing else). |

**Matching your IdP.** Set `SESSION_TTL` and `SESSION_IDLE` to match your
IdP's session policy. Simple Host re-checks the IdP only at sign-in, so these
are how long a leaver can keep working after being disabled at the IdP: a
browser session for up to `SESSION_TTL` (less if idle), a connected AI app for
up to `OAUTH_REFRESH_TTL` (each access token lasting `OAUTH_ACCESS_TTL`), and
an API key until it expires. Disabling the person in Simple Host as well (the
`/admin` page, or an offboard key; see INSTALL.md, "Sessions and leavers")
ends all three at once.

## Database

Either `DB_DSN` (a complete URL) or the four parts below, not a mix.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `DB_DSN` | One of this or the parts group | none | Must be a `postgres://` or `postgresql://` URL — a keyword/value DSN (`host=... sslmode=...`) is refused outright, not merely tolerated, because a crafted keyword/value string can otherwise smuggle a fake `sslmode=verify-full` past the TLS check while `lib/pq` itself connects with a real, different `sslmode` elsewhere in the same string. Re-parsed and re-serialized once at load time so the exact string the TLS check reads is byte-for-byte the string used to connect. |
| `DB_HOST` | Yes, if no `DB_DSN` | none | — |
| `DB_PORT` | No | `5432` | — |
| `DB_USER` | Yes, if no `DB_DSN` (and, for the server, no `DB_APP_USER`) | none | This is the **owning** role the `migrate` and `prune` subcommands connect as. The server does not use it when `DB_APP_USER` is set, which the Deployment manifest does. |
| `DB_PASSWORD` | Yes, if no `DB_DSN` (and, for the server, no `DB_APP_USER`) | none | Owning-role password; `DB_PASSWORD_FILE` is read instead when set. Must differ from `DB_APP_PASSWORD`: `migrate` and the server refuse to start when the two are equal. The manifest blanks it in the server container, so the owning password is not in the server's environment. |
| `DB_NAME` | Yes, if no `DB_DSN` | none | — |
| `DB_SSLMODE` | No | `verify-full` | Anything other than `verify-full` is refused unless `DB_INSECURE_ALLOWED=true`. |
| `DB_SSL_ROOT_CERT` | Required in practice with `sslmode=verify-full` | none | `verify-full` with no root certificate configured is refused unless `DB_INSECURE_ALLOWED=true`. Renamed from `DB_SSLROOTCERT`; the old name is not read. |
| `DB_INSECURE_ALLOWED` | No | `false` | Bypasses both TLS refusals above. For a local evaluation cluster only — never set on a real install. |
| `DB_INCLUSTER_EVALUATION` | No | `false` | Set by `deploy/components/postgres-incluster`. The server logs a warning at every start: that database has no backup. Evaluation only. |
| `DB_APP_PASSWORD` | Yes, whenever `migrate` runs, and for the server with `DB_APP_USER` | none | `DB_APP_PASSWORD_FILE` is read instead when set. The `migrate` subcommand sets this as the least-privilege application role's login password on every run, applied migrations or not, so a rotated value takes effect without a schema change. A role granted in a migration with no password to give it would otherwise sit unusable. Only a SCRAM-SHA-256 hash computed by `migrate` reaches the database, so the password never appears in server statement logs; it must be printable ASCII (the generated one is hex). |
| `DB_APP_USER` | No (set to `simplehost_app` by the Deployment manifest) | none | The role the **server** connects as. When set, the server builds its connection from `DB_HOST`/`DB_PORT`/`DB_NAME`, this user and `DB_APP_PASSWORD[_FILE]`, whatever `DB_USER`/`DB_PASSWORD[_FILE]` say. Refused together with `DB_DSN` (put the application role in the DSN itself). |

Whichever way the server's connection is configured, it checks the role it
got at startup and refuses to run if that role owns `audit_events`, can act
as its owner (a superuser included), or holds `UPDATE`, `DELETE` or
`TRUNCATE` on it — the owning role is for `migrate` and `prune` only.
A `DB_DSN` that fails to parse is reported without echoing the value, since
it may contain a password.

## Assets

Bounds on `POST /api/sites/{site}/assets`. None of the
three has a default that identifies an installation — these are plan/tier
knobs, not secrets, so all three fall back to the defaults below rather than refusing to start.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `ASSET_MAX_FILE_BYTES` | No | `26214400` (25 MiB) | Must parse as a positive integer. |
| `ASSET_MAX_SITE_BYTES` | No | `524288000` (500 MiB) | Must parse as a positive integer. Enforced against the site's live asset rows under a per-site lock, so concurrent uploads on any replica cannot both pass. |
| `ASSET_MAX_SITE_COUNT` | No | `5000` | Must parse as a positive integer. |

None of the shipped overlays (`local`, `byo`) set these; they are left at
their code defaults unless an installation has a reason to raise or lower
them.

## Retention and visibility (audit sink)

Loaded by `config.LoadAuditRetention()`, the same narrow-loader shape
`LoadDatabase`/`LoadAppRolePassword` use: the `prune` subcommand
(`cmd/server/subcommands.go`'s `runPrune`) calls it directly since it needs
only these three values plus `config.LoadDatabase`'s own DSN, and
`config.Load()` calls it too, so the full server sees the same values
through `cfg.Audit`. `prune` runs from `deploy/base/cronjob-prune.yaml`, a
monthly `CronJob` wired into `deploy/base/kustomization.yaml`'s
`resources:` list, under the database's owning role — the least-privilege
`simplehost_app` role has no `DELETE`/`DROP` on `audit_events`/
`access_log` at all, so retention can only ever run as the
owning role, never from the server's own connection pool.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `AUDIT_RETENTION_DAYS` | No | `400` | Must be a positive integer. |
| `ACCESS_LOG_RETENTION_DAYS` | No | `90` | Must be a positive integer. Also how long `prune` keeps a session (and its IP and user agent) after it expired or was signed out. |
| `ACCESS_LOG_VISIBILITY` | No | `counts` | Must be `counts`, `owner` or `admin`. `counts` (the default) answers a site's owner and team members on `GET /api/access` with views per day and the number of distinct viewers, never who; `owner` gives them each visit with the viewer's user id (IP and user agent stay admin-only); `admin` refuses every non-admin caller of that route outright. Admins always see full rows. Read by `handler.NewAuditHandler` (`cmd/server/main.go`); does not affect `GET /api/audit`, which is always scoped by caller identity rather than gated by this switch. |

## Streaming the audit log to a SIEM

Nothing to configure. After each audit row is written, the server writes
the same event to stdout as one JSON line, alongside the request log's
lines (the two share one writer, so lines never interleave):

```json
{"time":"...","level":"INFO","msg":"audit","type":"audit","at":"2026-09-26T10:00:00.123Z","action":"key_mint","actor_id":"...","actor_kind":"person","key_id":"","owner_id":"","site_id":"","team_id":"","ip":"10.0.0.9","user_agent":"...","request_id":"...","detail":{"note":"abc123"}}
```

`type` is always `"audit"` and the field names are stable; an empty string
means the field does not apply. `at` is the server's clock at write time
(the row's own `at` is the database's). A line is written only after the
database write succeeded, but an event recorded inside a larger
transaction is streamed before that transaction commits, so the database
(and its hash chain, `simple-host audit-verify`) is authoritative. Every
`state_write` is streamed, including the ones the database coalesces into
one row per five-minute window.

Forward it with whatever log shipper the cluster already runs (Fluent Bit,
Vector, the OpenTelemetry Collector, the Datadog Agent, Splunk OTel, Elastic
Agent): tail the `simple-host` pods' stdout (label `app=simple-host`),
parse each line as JSON, and keep the lines where `type` is `audit`.
Fluent Bit, after its `kubernetes` filter with `Merge_Log On`:

```ini
[FILTER]
    Name   grep
    Match  kube.*
    Regex  type ^audit$
```

Vector, on a `kubernetes_logs` source named `k8s`:

```toml
[transforms.simple_host_audit]
type = "remap"
inputs = ["k8s"]
drop_on_abort = true
source = """
. = object!(parse_json!(string!(.message)))
if .type != "audit" { abort }
"""
```

## Site bucket (S3-compatible)

The bucket is the site store: every site version and asset lives here, not on
the pod. The variable names keep their `BACKUP_` prefix. Requirements, key
layout and versioning: `docs/storage.md`.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `BACKUP_STORAGE_ENDPOINT` | Yes | none | Must be an absolute URL with a host; scheme must be `http` or `https`; `http` is refused unless `BACKUP_STORAGE_INSECURE_ALLOWED=true`. |
| `BACKUP_STORAGE_REGION` | Yes | none | — |
| `BACKUP_STORAGE_BUCKET` | Yes | none | — |
| `BACKUP_STORAGE_PREFIX` | No | `backups/` | — |
| `BACKUP_STORAGE_ACCESS_KEY_ID` | No | none | Must be set together with `BACKUP_STORAGE_SECRET_ACCESS_KEY` — exactly one of the pair is refused. Leave both unset to use the SDK's default credential chain (a platform's workload identity). |
| `BACKUP_STORAGE_SECRET_ACCESS_KEY` | No | none | See `BACKUP_STORAGE_ACCESS_KEY_ID`. |
| `BACKUP_STORAGE_INSECURE_ALLOWED` | No | `false` | Permits a plain-`http` `BACKUP_STORAGE_ENDPOINT`. For a local cluster where the bucket sits on the same node only; the local overlay's MinIO sets this. |
| `BACKUP_SSE` | No | `AES256` | Must be `AES256` or `aws:kms`. `aws:kms` requires `BACKUP_SSE_KEY_ID`; `AES256` refuses it being set. Sent as `x-amz-server-side-encryption` on every `PutObject`. Self-hosted MinIO refuses any value here without its own `MINIO_KMS_SECRET_KEY` configured on the MinIO side — see `docs/install.md` section 9. |
| `BACKUP_SSE_KEY_ID` | Required with `BACKUP_SSE=aws:kms` | none | See `BACKUP_SSE`. |
| `BACKUP_ENVELOPE_KEY` | No | none (envelope disabled) | Optional client-side envelope encryption, on top of the SSE header above (design 9.1). Up to eight comma-separated `<id>:<base64 32-byte key>` entries, same shape as `SESSION_SIGNING_KEY`: the first wraps every new object, every configured key is tried to unwrap an existing one. Rotation: add the new key second, deploy, swap the order, deploy — and never remove the old key, since stored objects are long-lived and every one wrapped under it would become unreadable. Set in a Secret; a bucket that is later fully compromised cannot read these objects without also having this key. Escrow it before first use: every site is readable only with it. With the envelope on, an object without one is refused (see the next row). |
| `BACKUP_ENVELOPE_PLAINTEXT_ALLOWED` | No | `false` | Lets an install that added `BACKUP_ENVELOPE_KEY` after it already held sites keep reading the objects written before the key. Otherwise, with the envelope on, an unenveloped object is refused as one only somebody with bucket access could have put there. `docs/storage.md` (Encryption). |

## Removed variables

These existed in the code this package was derived from and have no
equivalent here; do not set them.

| Variable | Why it is gone |
|---|---|
| `ADMIN_API_KEY` | Removed. There is no synthetic admin principal; admin status follows `ADMIN_EMAILS`/`OIDC_ADMIN_CLAIM` on a real signed-in person. |
| `AWS_SECRET_NAME` | The Secrets Manager config loader was removed; every secret is a Kubernetes Secret, materialized directly or through your External Secrets Operator integration. |
| `SUBDOMAIN_CUTOVER`, `OWNER_HOST_MANAGEMENT` | The subdomain-only serving shape is the only shape; there is no legacy path or cutover flag to flip. |

## Where these are set in the shipped overlays

- `deploy/overlays/local/config.env` and `secrets.env` — generated values
  for the local cluster; `secrets.env` is written by `make local-secrets`
  and gitignored.
- `deploy/overlays/byo/config.env.example` and `secrets.env.example` — copy
  to `config.env`/`secrets.env` and fill in for a real cluster; see
  `docs/install.md` section 5.
- `deploy/overlays/staging`, `deploy/overlays/production` — reuse `byo`'s
  `.example` files by convention; they have none of their own yet.

Neither overlay sets the Assets or Retention variables above; both are
left at their code defaults unless an installation overrides them.

Editing `config.env` or `secrets.env` and re-applying does not restart the
pods (the generated ConfigMap and Secret keep fixed names). Run
`kubectl -n <namespace> rollout restart deploy/simple-host` afterwards.

`docs/cloud/aws.md`, `gcp.md`, `azure.md`, and `oci.md` show where the
database, bucket, and certificate values for a specific cloud's managed
services map onto this table.
