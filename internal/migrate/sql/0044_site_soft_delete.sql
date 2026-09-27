-- Deleting a site keeps its row for a recovery window: deleted_at marks it
-- deleted (it stops serving and drops out of every listing at once), and the
-- row keeps its saved data, history, access level, viewers and asset records
-- so a restore brings all of it back. The name stays held until the row is
-- purged. deleted_by is the person who deleted it (no foreign key: the row
-- must outlive any change to that account).
--
-- Not marked backward-compatible on purpose: a binary from before this
-- column would serve every site in its recovery window again.
--
-- Lock duration: the ALTER takes ACCESS EXCLUSIVE on sites and the index
-- build below keeps it for one scan of the table, so site reads and writes
-- wait for that scan. It is one transaction on purpose (the migrator runs
-- each file in a transaction and has no non-transactional step for CREATE
-- INDEX CONCURRENTLY): a company's sites table holds one row per site,
-- thousands at most, which scans in milliseconds; the new column is all
-- NULL and the index is partial on NOT NULL, so it is built empty. The 30s
-- statement timeout bounds the worst case: a much larger table rolls the
-- whole file back instead of holding the lock longer.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS deleted_by uuid;

CREATE INDEX IF NOT EXISTS sites_deleted_at_idx ON sites (deleted_at) WHERE deleted_at IS NOT NULL;

COMMIT;
