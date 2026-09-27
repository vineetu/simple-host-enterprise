-- Deleting a site keeps its row for a recovery window: deleted_at marks it
-- deleted (it stops serving and drops out of every listing at once), and the
-- row keeps its saved data, history, access level, viewers and asset records
-- so a restore brings all of it back. The name stays held until the row is
-- purged. deleted_by is the person who deleted it (no foreign key: the row
-- must outlive any change to that account).
--
-- Not marked backward-compatible on purpose: a binary from before this
-- column would serve every site in its recovery window again.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS deleted_by uuid;

CREATE INDEX IF NOT EXISTS sites_deleted_at_idx ON sites (deleted_at) WHERE deleted_at IS NOT NULL;

COMMIT;
