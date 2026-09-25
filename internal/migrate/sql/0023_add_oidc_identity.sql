-- OIDC identity columns.
--
-- oidc_sub is the provider's stable subject claim, unique per account: once
-- a sign-in has bound an account to a subject, every later sign-in from that
-- provider account finds the same row by oidc_sub and never re-binds by
-- email. Nullable, because a person created before this release (or a team,
-- which never signs in) has none yet — the callback binds it once, by
-- email, only when the email's domain is in ALLOWED_EMAIL_DOMAINS.
--
-- disabled_at is the offboarding column: an admin disabling a person
-- sets it, every session and API key of theirs is revoked in the same
-- action, and a later sign-in is refused even though the provider would
-- still allow it. Their sites keep serving; nothing here touches sites.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_sub text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS users_oidc_sub_idx ON users (oidc_sub) WHERE oidc_sub IS NOT NULL;

COMMIT;
