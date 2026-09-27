# Accounts and sign-in

People sign in only through your OIDC provider; an account is created at first sign-in. There
are no passwords and no admin key. **Admins** come from `ADMIN_EMAILS` or an OIDC claim
(`OIDC_ADMIN_CLAIM` = `OIDC_ADMIN_VALUE`), refreshed at every sign-in. With Google, set
`ALLOWED_EMAIL_DOMAINS`, or anyone with a Google account could sign in (the server refuses to
start without it).

**Sessions** have an absolute and an idle limit. Simple Host checks the identity provider only at
sign-in, so these are how long a leaver can keep working after being disabled there; disabling
the person in `/admin` ends everything at once.

**AI apps** connect to `/mcp` through the same sign-in and keep a short access token, refreshed
until `OAUTH_REFRESH_TTL` after that sign-in.

**API keys** are for CI and automation, with a scope and an expiry.

<!-- settings:group=accounts -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `OIDC_ISSUER` | none | text | Your identity provider's issuer URL (Okta, Entra ID tenant, Google, Keycloak, ...). People sign in only through it. **Required.** **Security-sensitive.** |
| `OIDC_CLIENT_ID` | none | text | The OIDC client ID registered for this install. **Required.** |
| `OIDC_CLIENT_SECRET` | none | secret | The OIDC client secret. **Required.** **Security-sensitive.** |
| `OIDC_SCOPES` | `openid email profile` | text | Scopes asked for at sign-in, space-separated. |
| `OIDC_EMAIL_CLAIM` | `email` | text | The ID token claim read as the person's email address. The provider must vouch that it is verified. |
| `OIDC_USERNAME_CLAIM` | none | text | A claim to derive usernames from. Empty: the part of the email before the @. |
| `OIDC_ADMIN_CLAIM` | none | text | A claim that makes someone an admin when it holds OIDC_ADMIN_VALUE. Set both or neither. **Security-sensitive.** |
| `OIDC_ADMIN_VALUE` | none | text | The value of OIDC_ADMIN_CLAIM that makes someone an admin. **Security-sensitive.** |
| `OIDC_INSECURE_ALLOWED` | `false` | `true` / `false` | Allows a plain-HTTP identity provider. Local evaluation only; never on a real install. **Security-sensitive.** |
| `ADMIN_EMAILS` | none | text | Admins' email addresses, comma-separated. Removing one demotes them on the next deploy. **Security-sensitive.** |
| `ALLOWED_EMAIL_DOMAINS` | none | text | Email domains that may sign in, comma-separated. Required with Google, where anyone has an account. **Security-sensitive.** |
| `OIDC_HINT_DOMAIN` | `<the only ALLOWED_EMAIL_DOMAINS entry>` | text | The domain hint sent to the provider, to narrow its account chooser. It never authorizes anyone. |
| `OAUTH_REDIRECT_HOSTS` | `chatgpt.com,claude.ai,vscode.dev,localhost,cursor://anysphere.cursor-mcp` | text | Where an AI app connecting to /mcp may be sent back after sign-in, comma-separated. **Security-sensitive.** |
| `SESSION_SIGNING_KEY` | none | secret | Signs the sign-in cookie: one or two <id>:<base64 32-byte key> entries, the first signing. Generate with echo "k1:$(openssl rand -base64 32)". **Required.** **Security-sensitive.** |
| `SESSION_TTL` | `8h` | at most 24h Go duration | How long a sign-in lasts, however active. Match your identity provider's session policy. **Security-sensitive.** |
| `SESSION_IDLE` | `30m` | at most 8h Go duration | How long a sign-in survives unused. Not longer than SESSION_TTL. **Security-sensitive.** |
| `OAUTH_ACCESS_TTL` | `1h` | at most 24h Go duration | How long an AI app's access token works before it refreshes. **Security-sensitive.** |
| `OAUTH_REFRESH_TTL` | `720h` | at most 2160h Go duration | How long an AI app stays connected before the person signs in at the identity provider again. **Security-sensitive.** |
| `API_KEY_MAX_DAYS` | `365` | 1–365 days | The longest lifetime an API key (for CI and automation) may be minted with. **Security-sensitive.** |
| `API_KEY_DEFAULT_DAYS` | `90` | 1–365 days | A new API key's lifetime when none is asked for. At most API_KEY_MAX_DAYS. **Security-sensitive.** |
| `API_KEY_EXPIRY_WARNING_DAYS` | `14` | 1–365 days | A key this close to expiry shows "expires soon" and warns on every call. |
| `RATE_LIMIT_AUTH_CLIENT` | `20/5s` | stricter freely; loosest `80/1.25s` | Sign-in, the session hand-off and AI app authorization, per client address (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_AUTH_EMAIL` | `5/50s` | stricter freely; loosest `20/12.5s` | Sign-in attempts per email address. **Security-sensitive.** |
| `RATE_LIMIT_API_KEY_MINT` | `30/10s` | stricter freely; loosest `120/2.5s` | API keys minted per person (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_OAUTH_REGISTER` | `30/10s` | stricter freely; loosest `120/2.5s` | AI app registrations per client address (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_OAUTH_TOKEN` | `120/500ms` | stricter freely; loosest `480/125ms` | AI app token requests per client address (counted across replicas). **Security-sensitive.** |
<!-- /settings -->

## Recipes

**Stricter sign-in** (a shorter working session, AI apps re-sign-in weekly, short-lived keys):

```
SESSION_TTL=4h
SESSION_IDLE=15m
OAUTH_REFRESH_TTL=168h
API_KEY_MAX_DAYS=30
API_KEY_DEFAULT_DAYS=30
RATE_LIMIT_AUTH_EMAIL=3/2m
```

**Admins from a group claim.** `OIDC_ADMIN_CLAIM=groups` and
`OIDC_ADMIN_VALUE=simple-host-admins`; keep `ADMIN_EMAILS` as a fallback.
