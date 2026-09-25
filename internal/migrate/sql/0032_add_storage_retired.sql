-- The bucket is the site store (internal/storage). Versions and assets are
-- immutable objects keyed by site id; the database decides which of them are
-- live. When a version or a whole site stops being referenced, its key (or
-- key prefix, ending in "/") is queued here in the same transaction, and a
-- sweep in the server deletes the objects once retire_after has passed. The
-- grace period lets a replica that resolved a version a moment earlier finish
-- serving it.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS storage_retired (
    id           bigserial PRIMARY KEY,
    object_key   text NOT NULL,
    retire_after timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS storage_retired_due_idx ON storage_retired (retire_after);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE storage_retired TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE storage_retired_id_seq TO simplehost_app;

COMMIT;
