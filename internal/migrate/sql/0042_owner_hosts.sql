-- simple-host: backward-compatible
-- Marked compatible because it only adds a table no older code reads.
--
-- One row per owner label whose "*.<owner>.<base>" certificate the owner-hosts
-- reconciler manages. ready is true once cert-manager reports that
-- certificate Ready; until then the server serves the owner's sites at
-- "<owner>.<base>/<site>/" rather than send anyone to a host with no
-- certificate. The reconciler connects as the application role.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS owner_hosts (
    owner_label text PRIMARY KEY,
    ready       boolean NOT NULL DEFAULT false,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE owner_hosts TO simplehost_app;

COMMIT;
