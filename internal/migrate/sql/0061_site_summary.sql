-- simple-host: backward-compatible
-- Summaries belong to immutable versions, including held deploys. Lists read
-- the live version, so viewers never receive unpublished content.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE versions ADD COLUMN summary jsonb;
COMMIT;
