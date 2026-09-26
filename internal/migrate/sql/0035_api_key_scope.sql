-- simple-host: backward-compatible
-- Marked compatible because it only adds a column with a default: v1.1.x code
-- never names api_keys.scope, its INSERTs take the default and its SELECTs
-- list their columns, so a rollback runs unchanged against this schema (and
-- simply stops enforcing scopes, which is what that release did anyway).
--
-- API keys get a scope, chosen when the key is minted and enforced by
-- internal/auth.Middleware right after the key lookup:
--   publish   deploy, update, roll back and list sites, their versions,
--             archives, saved data and assets; /api/me, /api/sites and /mcp
--             (the default for a new key: what CI needs and nothing more)
--   full      everything the person can do through the REST API, never the
--             admin routes
--   offboard  exactly one route, POST /api/admin/users/disable, and only
--             for a key an admin minted (HR automation for leavers)
-- Every key that already exists becomes 'full', so CI that works today keeps
-- working; the column default is then 'publish' for every key minted after.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'full';
ALTER TABLE api_keys ALTER COLUMN scope SET DEFAULT 'publish';
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scope_check CHECK (scope IN ('publish', 'full', 'offboard'));

COMMIT;
