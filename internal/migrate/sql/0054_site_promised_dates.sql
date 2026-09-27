-- simple-host: backward-compatible
-- Marked compatible because it only adds two nullable columns older code
-- ignores: an older binary goes back to working the dates out from the
-- settings in force.
--
-- The date a site's owner was given is stored when it is given, so a later
-- change to DELETED_RETENTION_DAYS or IDLE_CLEANUP_GRACE_DAYS applies to new
-- deletions and new idle marks only:
--   sites.purge_at        a deleted site stops being recoverable (set on delete)
--   sites.idle_delete_at  a site marked idle moves to Recently deleted (set on the mark)
-- NULL (rows from before this migration) falls back to the setting.
--
-- Lock duration: two ADD COLUMNs with no default, catalog-only changes.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS purge_at timestamptz,
    ADD COLUMN IF NOT EXISTS idle_delete_at timestamptz;

COMMIT;
