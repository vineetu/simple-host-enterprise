# Configuration reference

DRAFT — reconcile after Phase 7.

Generated from `internal/config/config.go` as of Phase 4, the last phase to
add a variable there — including the audit sink's retention and visibility
settings, which now load through `config.LoadAuditRetention()` alongside
everything else rather than being read directly from the environment.
Everything under "Public address" through "Retention" is read by
`config.Load()` (the server) or `config.LoadDatabase()` /
`config.LoadAppRolePassword()` / `config.LoadAuditRetention()` (the
`migrate` and `prune` subcommands, which each need only a narrower slice of
the same environment). Nothing that identifies a particular installation
has a default: leave any of the required rows below unset and the process
refuses to start, reporting every missing name at once in a single
`missing required configuration: ...` error, not one restart per gap.

Phases 0, 1, 2, 3, 4, and 5 are reflected here in full.


## Secrets: environment or file

Every secret below can be given directly, or through a file: set
`<NAME>_FILE` to a path and the value is read from there instead, with a
trailing newline trimmed. The file wins when both are set. A `_FILE` that
cannot be read stops startup with its own error rather than being reported as
a missing value — a broken mount and an unset variable need different fixes.

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
| `SITE_DIR` | No | `/mnt/data/sites` | none |
| `RESERVED_LABELS` | No | none (empty) | Comma-separated; extends the built-in reserved-label set (`www`, `api`, `admin`, `sites`, `mcp`, `docs`, `auth`, `login`, `mail`, `cdn`, `status`, `app`, and the rest — design 7.1, `internal/handler/names.go`) with installation-specific hostnames that must never belong to an account or a site. Checked at account and site creation, and (Phase 2) any label containing `--` anywhere is refused outright, independent of this list, since that shape is reserved for a restricted site's own hostname (`<owner>--<site>.<base>`, design 5.2a). |

## Identity (OIDC)

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `OIDC_ISSUER` | Yes | none | Discovery and JWKS are fetched from this issuer at startup; sign-in fails if it is unreachable or its metadata does not match. |
| `OIDC_CLIENT_ID` | Yes | none | none at startup; the provider refuses the authorization request if wrong. |
| `OIDC_CLIENT_SECRET` | Yes | none | none at startup; token exchange fails if wrong. Belongs in a Secret, never in `config.env`. |
| `OIDC_SCOPES` | No | `openid email profile` | Space-separated. |
| `OIDC_EMAIL_CLAIM` | No | `email` | Overrides the ID token claim read as the person's address, for a provider that does not use `email`. |
| `OIDC_USERNAME_CLAIM` | No | none (derives from the email's local part) | When set, this claim's value is used to derive the account's username instead. |
| `OIDC_ADMIN_CLAIM` | No | none | Must be set together with `OIDC_ADMIN_VALUE` — setting exactly one of the pair is refused at startup. |
| `OIDC_ADMIN_VALUE` | No | none | See `OIDC_ADMIN_CLAIM`. |
| `ADMIN_EMAILS` | No | none (empty) | Comma-separated, lowercased. A person is admin if their address is in this list, or `OIDC_ADMIN_CLAIM`/`OIDC_ADMIN_VALUE` matches (either source grants it), refreshed on every sign-in. The portable admin path every reference install documents — neither Google nor Entra's common endpoint puts groups in the ID token. |
| `ALLOWED_EMAIL_DOMAINS` | No | none (empty, meaning unrestricted) | Comma-separated, lowercased. Required in practice for a multi-tenant provider (Google, Entra's common endpoint): without it, anyone with an account at that provider can sign in. Also gates whether an existing row may be claimed by a new sign-in's email (design 6.1) — without this list, that claim path never runs. |
| `OIDC_HINT_DOMAIN` | No | the sole `ALLOWED_EMAIL_DOMAINS` entry, if there is exactly one | Sent as the provider's domain hint (Google: `hd`) on the authorization request. A hint narrows the account chooser; it never authorizes — the callback still checks the claim and the domain list independently. |
| `OAUTH_REDIRECT_HOSTS` | No | `chatgpt.com,claude.ai,vscode.dev,localhost,cursor://anysphere.cursor-mcp` | Where an AI app connecting to `/mcp` over OAuth may be sent back after sign-in. Comma-separated: a hostname allows `https` redirects to it, `localhost` allows loopback redirects for apps on the person's own machine (Claude Code, Codex), `scheme://host` allows an app's own URL scheme, and `*` allows any `https` host. Apps register themselves; this list is what limits which ones can finish connecting. |

## Sessions

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `SESSION_SIGNING_KEY` | Yes | none | One or two comma-separated `<id>:<base64 32-byte key>` entries. More than two, a duplicate id, a non-base64 value, or a decoded length other than 32 bytes is refused. The `__Host-` session cookie has no insecure fallback, so this is required even on a rehearsal install. Rotation: add the new key second, deploy, swap the order so it signs, deploy, remove the old key after `SESSION_TTL` has fully elapsed. |
| `SESSION_TTL` | No | `12h` | Must parse as a positive Go duration. |
| `SESSION_IDLE` | No | `1h` | Must parse as a positive Go duration. |

## Database

Either `DB_DSN` (a complete URL) or the four parts below, not a mix.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `DB_DSN` | One of this or the parts group | none | Must be a `postgres://` or `postgresql://` URL — a keyword/value DSN (`host=... sslmode=...`) is refused outright, not merely tolerated, because a crafted keyword/value string can otherwise smuggle a fake `sslmode=verify-full` past the TLS check while `lib/pq` itself connects with a real, different `sslmode` elsewhere in the same string. Re-parsed and re-serialized once at load time so the exact string the TLS check reads is byte-for-byte the string used to connect. |
| `DB_HOST` | Yes, if no `DB_DSN` | none | — |
| `DB_PORT` | No | `5432` | — |
| `DB_USER` | Yes, if no `DB_DSN` | none | This is the **owning** role the `migrate` subcommand and its init container connect as (design 9.3); the server itself is given `simplehost_app` and `DB_APP_PASSWORD` as an explicit override in the Deployment manifest, which wins over whatever `DB_USER`/`DB_PASSWORD` this section supplies. |
| `DB_PASSWORD` | Yes, if no `DB_DSN` | none | Owning-role password. Must differ from `DB_APP_PASSWORD` — the design requires the two roles never share a password, though this is not currently machine-checked. |
| `DB_NAME` | Yes, if no `DB_DSN` | none | — |
| `DB_SSLMODE` | No | `verify-full` | Anything other than `verify-full` is refused unless `DB_INSECURE_ALLOWED=true`. |
| `DB_SSL_ROOT_CERT` | Required in practice with `sslmode=verify-full` | none | `verify-full` with no root certificate configured is refused unless `DB_INSECURE_ALLOWED=true`. Renamed from `DB_SSLROOTCERT` after Phase 0; see that phase's implementation record if you find the old name in an older note. |
| `DB_INSECURE_ALLOWED` | No | `false` | Bypasses both TLS refusals above. For a local evaluation cluster only — never set on a real install. |
| `DB_APP_PASSWORD` | Yes, whenever `migrate` runs | none | Read by `config.LoadAppRolePassword()`. The `migrate` subcommand sets this as the least-privilege application role's login password on every run, applied migrations or not, so a rotated value takes effect without a schema change. A role granted in a migration with no password to give it would otherwise sit unusable. |

## Assets (design.md 7.3)

Bounds on `POST /api/sites/{site}/assets`, added in Phase 3. None of the
three has a default that identifies an installation — these are plan/tier
knobs, not secrets, so all three fall back to design 7.3's own reference
numbers rather than refusing to start.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `ASSET_MAX_FILE_BYTES` | No | `26214400` (25 MiB) | Must parse as a positive integer. |
| `ASSET_MAX_SITE_BYTES` | No | `524288000` (500 MiB) | Must parse as a positive integer. Enforced by walking the site's `assets/` directory on disk at upload time, not by a stored counter, so it stays correct even if a row and its file ever drifted apart. |
| `ASSET_MAX_SITE_COUNT` | No | `5000` | Must parse as a positive integer. |

None of the shipped overlays (`local`, `byo`) set these; they are left at
their code defaults unless an installation has a reason to raise or lower
them.

## Retention and visibility (audit sink, design.md 8.2)

Loaded by `config.LoadAuditRetention()`, the same narrow-loader shape
`LoadDatabase`/`LoadAppRolePassword` use: the `prune` subcommand
(`cmd/server/subcommands.go`'s `runPrune`) calls it directly since it needs
only these three values plus `config.LoadDatabase`'s own DSN, and
`config.Load()` calls it too, so the full server sees the same values
through `cfg.Audit`. `prune` runs from `deploy/base/cronjob-prune.yaml`, a
monthly `CronJob` wired into `deploy/base/kustomization.yaml`'s
`resources:` list, under the database's owning role — the least-privilege
`simplehost_app` role has no `DELETE`/`DROP` on `audit_events`/
`access_log` at all (design 9.3), so retention can only ever run as the
owning role, never from the server's own connection pool.

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `AUDIT_RETENTION_DAYS` | No | `400` | Must be a positive integer. |
| `ACCESS_LOG_RETENTION_DAYS` | No | `90` | Must be a positive integer. |
| `ACCESS_LOG_VISIBILITY` | No | `owner` | Must be `owner` or `admin`. `owner` (design 8.2's default) lets a site's owner and team members read `GET /api/access` for their own sites; `admin` refuses every non-admin caller of that route outright, regardless of ownership. Read by `handler.NewAuditHandler` (`cmd/server/main.go`) on every request; does not affect `GET /api/audit`, which is always scoped by caller identity rather than gated by this switch. |

## Backup bucket (S3-compatible)

| Variable | Required | Default | Refusal it triggers when set wrong |
|---|---|---|---|
| `BACKUP_STORAGE_ENDPOINT` | Yes | none | Must be an absolute URL with a host; scheme must be `http` or `https`; `http` is refused unless `BACKUP_STORAGE_INSECURE_ALLOWED=true`. |
| `BACKUP_STORAGE_REGION` | Yes | none | — |
| `BACKUP_STORAGE_BUCKET` | Yes | none | — |
| `BACKUP_STORAGE_PREFIX` | No | `backups/` | — |
| `BACKUP_STORAGE_ACCESS_KEY_ID` | No | none | Must be set together with `BACKUP_STORAGE_SECRET_ACCESS_KEY` — exactly one of the pair is refused. Leave both unset to use the SDK's default credential chain (a platform's workload identity). |
| `BACKUP_STORAGE_SECRET_ACCESS_KEY` | No | none | See `BACKUP_STORAGE_ACCESS_KEY_ID`. |
| `BACKUP_STORAGE_INSECURE_ALLOWED` | No | `false` | Permits a plain-`http` `BACKUP_STORAGE_ENDPOINT`. For a local cluster where the bucket sits on the same node only; the local overlay's MinIO sets this. |
| `BACKUP_SSE` | No | `AES256` | Must be `AES256` or `aws:kms`. `aws:kms` requires `BACKUP_SSE_KEY_ID`; `AES256` refuses it being set. Sent as `x-amz-server-side-encryption` on every backup `PutObject`. Self-hosted MinIO refuses any value here without its own `MINIO_KMS_SECRET_KEY` configured on the MinIO side — see `docs/install.md` section 9. |
| `BACKUP_SSE_KEY_ID` | Required with `BACKUP_SSE=aws:kms` | none | See `BACKUP_SSE`. |
| `BACKUP_ENVELOPE_KEY` | No | none (envelope disabled) | Optional client-side envelope encryption, on top of the SSE header above (design 9.1). One or two comma-separated `<id>:<base64 32-byte key>` entries, same shape and rotation procedure as `SESSION_SIGNING_KEY`: the first wraps every new backup object, every configured key is tried to unwrap an existing one. Set in a Secret; a bucket that is later fully compromised cannot read these objects without also having this key. |

## Removed since the source instance

These existed in the original internal instance and have no equivalent
here; do not set them.

| Variable | Why it is gone |
|---|---|
| `ADMIN_API_KEY` | Removed in Phase 1. There is no synthetic admin principal; admin status follows `ADMIN_EMAILS`/`OIDC_ADMIN_CLAIM` on a real signed-in person. |
| `AWS_SECRET_NAME` | The Secrets Manager config loader was removed in Phase 0; every secret is a Kubernetes Secret, materialized directly or through your External Secrets Operator integration. |
| `SUBDOMAIN_CUTOVER`, `OWNER_HOST_MANAGEMENT` | The subdomain-only serving shape is the only shape (Phase 2); there is no legacy path or cutover flag to flip. |

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

`docs/cloud/aws.md`, `gke.md`, and `aks.md` show where the database, bucket,
and certificate values for a specific cloud's managed services map onto
this table.
