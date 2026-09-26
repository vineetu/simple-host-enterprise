-- simple-host: backward-compatible
-- Marked compatible because it only adds a nullable column and a partial
-- index: older binaries never name versions.size_bytes, insert versions
-- without it (it stays NULL) and read them unchanged.
--
-- versions.size_bytes is the stored size of a version's archive in the
-- bucket, recorded at deploy. Summed with site_assets.size it is the stored
-- bytes an owner's quota (QUOTA_MAX_BYTES) counts. Versions written before
-- this column, or by the restore and migrate-storage subcommands, start NULL;
-- the server fills them in from the bucket in the background (the partial
-- index keeps finding them cheap once there are none left).

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE versions ADD COLUMN IF NOT EXISTS size_bytes bigint;

CREATE INDEX IF NOT EXISTS versions_size_unknown_idx ON versions (id) WHERE size_bytes IS NULL;

COMMIT;
