# Email

Simple Host Enterprise sends email only for idle-site cleanup: the owner (or a team's members)
is told when a site is marked. Sign-in goes through your identity provider, so there are no
sign-in emails. Without `SMTP_URL` the dashboard notice and the admin list are the only notice.

The relay must offer STARTTLS on `smtp://` (add `?insecure=1` only for an internal relay without
TLS) or use `smtps://`. `SMTP_URL` carries a password, so it goes in the Secret.

<!-- settings:group=email -->
| Setting | Default | Allowed | What it does |
|---|---|---|---|
| `SMTP_URL` | none | secret | Your mail relay, smtp://user:password@host:587 (STARTTLS) or smtps://host:465. It carries a password, so it goes in the Secret. **Security-sensitive.** |
| `SMTP_FROM` | none | text | The sender, like Simple Host <hosting@example.com>. Required with SMTP_URL. |
<!-- /settings -->
