-- Sessions: one row per sign-in, shared by every host cookie minted from it
-- (design.md 6.1). Revoking this row, or disabling the user, signs the
-- person out everywhere within the cache window described there.
--
-- The cookie itself never carries these columns in the clear: it carries a
-- signed (session_id, user_id, exp) payload, and this table is what a
-- revocation or an idle check actually consults.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS sessions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    ip            inet,
    user_agent    text,
    revoked_at    timestamptz
);

-- Every session lookup is by id (cookie verification) or by user (the
-- sessions list, and the sign-out-everywhere path on disable/offboard).
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE sessions TO simplehost_app;

COMMIT;
