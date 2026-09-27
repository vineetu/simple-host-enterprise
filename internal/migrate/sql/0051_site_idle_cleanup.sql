-- simple-host: backward-compatible
-- Marked compatible because it only adds columns older code ignores: an
-- older binary neither marks nor deletes idle sites, and keeps serving
-- every site as before.
--
-- The opt-in idle-site cleanup (IDLE_CLEANUP_DAYS). idle_since is when the
-- cleanup marked a site nobody had visited or deployed for that many days;
-- 30 days later, still unused, it moves to Recently deleted. Any visit or
-- deploy, or Keep, clears it. idle_keep is Keep: the owner or a team member
-- said this site stays, and the cleanup never marks it again.
--
-- Lock duration: two ADD COLUMNs, one with a constant default, which
-- Postgres 11+ records in the catalog without rewriting the table.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS idle_since timestamptz,
    ADD COLUMN IF NOT EXISTS idle_keep boolean NOT NULL DEFAULT false;

COMMIT;
