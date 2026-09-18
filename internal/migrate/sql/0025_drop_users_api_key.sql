-- Drop users.api_key (Phase 1 review, folded into Phase 2's one-way schema
-- window; design.md 10.2's expand/contract).
--
-- Phase 1 stopped the request-authentication path reading this column
-- (internal/auth.Middleware hashes X-API-Key and looks up api_keys instead)
-- and backfilled every existing value into that table. What the review
-- found still reading it live was the pre-OIDC "paste your key" sign-in
-- form (deleted in Phase 2 along with the rest of the base-host per-user
-- page) and its GetUserByAPIKey query (a plaintext, unsalted equality
-- compare against this column — a key revoked through /api/keys kept
-- working through that form, since revocation only ever touched api_keys).
-- Every other read of this column across internal/db was a harmless
-- COALESCE(api_key, '') carried along for no live purpose; all are removed
-- in the same commit as this migration, so nothing in the binary this
-- migration ships with still expects the column to exist.
--
-- users_teams_have_no_key (0019_add_teams.sql) is BEFORE INSERT OR UPDATE OF
-- api_key, kind ON users — a trigger naming a column directly blocks
-- dropping that column, so it and its function are dropped first. The
-- invariant it protected (a team namespace holds no credential) no longer
-- needs database enforcement once there is no column left for one to occupy.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

DROP TRIGGER IF EXISTS users_teams_have_no_key ON users;
DROP FUNCTION IF EXISTS teams_have_no_key();

ALTER TABLE users DROP COLUMN IF EXISTS api_key;

COMMIT;
