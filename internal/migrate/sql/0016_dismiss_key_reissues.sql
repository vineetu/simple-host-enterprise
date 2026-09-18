-- Let an admin clear a reviewed re-issue off the dashboard.
--
-- The row stays: it is the audit trail, and deleting it would lose the record
-- of a credential handed out without approval. Dismissing only means somebody
-- has looked at it.

ALTER TABLE key_reissues ADD COLUMN IF NOT EXISTS reviewed_at timestamptz;

CREATE INDEX IF NOT EXISTS idx_key_reissues_unreviewed
    ON key_reissues (created_at DESC)
    WHERE reviewed_at IS NULL;
