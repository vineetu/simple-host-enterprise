# Rate limits

Each rate limit is `RATE_LIMIT_<NAME>=<burst>/<interval>`: that many requests at once, then one
more every `<interval>` (a Go duration, `1ms` to `1h`). `RATE_LIMIT_AUTH_EMAIL=5/50s` allows five
sign-in attempts for one address at once, then one every 50 seconds.

**Security-sensitive** limits (sign-in, API key mint, admin actions, the connector's OAuth) can
be made stricter freely but at most four times looser than built in; anything looser stops the
server at startup. The others may be set to anything, with a startup warning past ten times
looser. Limits counted across replicas are counted in Postgres, in a window of burst times
interval of at most 30 minutes; the rest are per replica.

<!-- settings:type=rate -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `RATE_LIMIT_AUTH_CLIENT` | `20/5s` | stricter freely; loosest `80/1.25s` | Sign-in, the session hand-off and AI app authorization, per client address (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_AUTH_EMAIL` | `5/50s` | stricter freely; loosest `20/12.5s` | Sign-in attempts per email address. **Security-sensitive.** |
| `RATE_LIMIT_API_KEY_MINT` | `30/10s` | stricter freely; loosest `120/2.5s` | API keys minted per person (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_OAUTH_REGISTER` | `30/10s` | stricter freely; loosest `120/2.5s` | AI app registrations per client address (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_OAUTH_TOKEN` | `120/500ms` | stricter freely; loosest `480/125ms` | AI app token requests per client address (counted across replicas). **Security-sensitive.** |
| `RATE_LIMIT_ADMIN_CLIENT` | `10/10s` | stricter freely; loosest `40/2.5s` | Admin actions per client address. **Security-sensitive.** |
| `RATE_LIMIT_ADMIN_IDENTITY` | `10/10s` | stricter freely; loosest `40/2.5s` | Admin actions per admin. **Security-sensitive.** |
| `RATE_LIMIT_MANAGEMENT_CLIENT` | `60/1s` | any (warns past 10× looser) | Management API calls per client address. |
| `RATE_LIMIT_MANAGEMENT_USER` | `30/10s` | any (warns past 10× looser) | Management changes per person. |
| `RATE_LIMIT_SEARCH_QUERY_PEER` | `200/50ms` | any (warns past 10× looser) | Site searches per network peer. |
| `RATE_LIMIT_SEARCH_QUERY_SESSION` | `60/1s` | any (warns past 10× looser) | Site searches per search session. |
| `RATE_LIMIT_STATE_CLIENT` | `60/1s` | any (warns past 10× looser) | Saved-data writes per client. |
| `RATE_LIMIT_STATE_SITE` | `60/1s` | any (warns past 10× looser) | Saved-data writes per site. |
| `RATE_LIMIT_STATE_READ_CLIENT` | `120/500ms` | any (warns past 10× looser) | Saved-data reads per client. |
| `RATE_LIMIT_STATE_READ_SITE` | `300/200ms` | any (warns past 10× looser) | Saved-data reads per site. |
<!-- /settings -->
