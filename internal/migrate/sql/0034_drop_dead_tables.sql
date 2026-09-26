-- simple-host: backward-compatible
-- Marked compatible because no v1.1.2 code reads or writes these tables, so a
-- rollback to it runs unchanged against the schema this leaves.
--
-- Tables left from features that no longer exist: reset_requests (0005, the
-- emailed key-reset intake), key_reissues (0015/0016, self-service key
-- re-issue) and ai_usage (0008, the model proxy's usage counters). Identity is
-- OIDC sign-in and there is no model proxy, so nothing writes them any more.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

DROP TABLE IF EXISTS reset_requests;
DROP TABLE IF EXISTS key_reissues;
DROP TABLE IF EXISTS ai_usage;

COMMIT;
