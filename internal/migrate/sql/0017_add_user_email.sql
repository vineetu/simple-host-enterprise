-- Record the email an account was created from.
--
-- Registration has always received an email and thrown it away, keeping only
-- the username derived from its local part. That leaves no way to tell which
-- human an account belongs to, which is how one person ends up with several
-- accounts nobody can reconcile, and how a typo becomes a permanent name.
--
-- The column is display and reconciliation data only. Nothing authenticates or
-- authorizes with it, and nothing matches on it; the API key remains the only
-- credential. `email_source` records how we came to believe it, so a guess is
-- never mistaken for something the account holder told us.
--
-- Additive and re-runnable. The running binary does not read these columns, so
-- this can be applied before the code that populates them.

BEGIN;

-- Fail fast rather than queue behind a long transaction while holding a lock
-- on users, which every authenticated request reads.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE users ADD COLUMN IF NOT EXISTS email text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_source text;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_source_check;
ALTER TABLE users ADD CONSTRAINT users_email_source_check CHECK (
    (email IS NULL) = (email_source IS NULL)
    AND (email_source IS NULL OR email_source IN ('registered', 'claimed', 'reset_request', 'inferred', 'admin'))
);

-- Deliberately NOT unique. Two accounts can legitimately share an address
-- today (a typo account and its owner), and a unique index would make this
-- migration fail rather than record what is actually true.
CREATE INDEX IF NOT EXISTS users_email_idx ON users (email) WHERE email IS NOT NULL;

-- 1. What we actually know: the address on a reset request an admin approved.
--    The requester proved nothing, but an admin looked at it and acted on it.
UPDATE users u
SET email = r.email, email_source = 'reset_request'
FROM (
    SELECT DISTINCT ON (user_id) user_id, email
    FROM reset_requests
    WHERE approved_at IS NOT NULL
    ORDER BY user_id, created_at DESC
) r
WHERE r.user_id = u.id AND u.email IS NULL;

-- 2. No inferred backfill. The original instance guessed an address from the
--    username and its corporate domain; a fresh install has no such domain to
--    guess, so accounts without a recorded address stay NULL until sign-in
--    records one.

COMMIT;
