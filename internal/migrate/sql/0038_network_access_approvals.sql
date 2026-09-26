-- simple-host: backward-compatible
-- Marked compatible because it only adds a table no older code reads or
-- writes: a rollback runs unchanged against the schema this leaves, and
-- approvals it leaves behind never count again (see below).
--
-- Admin approvals of a pending network-access request, for
-- NETWORK_ACCESS_APPROVALS=2 (two different admins must approve). A row is
-- one admin's approval of one request, and a request is identified by the
-- site and its sites.network_requested_at: a new request gets a new
-- timestamp, so it starts from zero even if older code, which never clears
-- this table, withdrew or replaced the previous one. The primary key makes
-- one admin's second approval of the same request a no-op.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS network_access_approvals (
    site_id      uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    requested_at timestamptz NOT NULL,
    admin_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    approved_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, requested_at, admin_id)
);

GRANT SELECT, INSERT, DELETE ON TABLE network_access_approvals TO simplehost_app;

COMMIT;
