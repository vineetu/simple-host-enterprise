-- Self-service key recovery.
--
-- A key that has never authenticated a request is not a secret worth
-- defending: nobody has a working setup built on it. Re-issuing one lets a
-- lost registration response heal itself instead of queueing for an admin.
-- Keys are also re-issuable during a short grace window after signup, when
-- somebody is plainly still setting up. Every such re-issue is recorded here
-- for later review rather than silently allowed.

ALTER TABLE users ADD COLUMN IF NOT EXISTS key_first_used_at timestamptz;

CREATE TABLE IF NOT EXISTS key_reissues (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_key_reissues_created_at
    ON key_reissues (created_at DESC);
