-- API keys, addressable and revocable one at a time.
--
-- Replaces the single users.api_key column as the credential an agent
-- presents in X-API-Key: a person may hold several, each named and each
-- revocable without touching the others. key_hash is the SHA-256 of the 32
-- random bytes the plaintext key is made of; SHA-256 with no salt is
-- adequate here because the key is 256 random bits, not a password an
-- attacker could feasibly enumerate. prefix is the first 8 hex characters,
-- kept only so a list of keys is recognisable in the UI; it grants nothing.
--
-- users.api_key is NOT dropped here. This release stops reading it from the
-- request-authentication path (internal/auth.Middleware hashes X-API-Key
-- and looks up this table instead); the column is backfilled into this
-- table below so an existing key keeps working unless its holder later
-- revokes it and mints a fresh one. Dropping users.api_key is a later,
-- one-way expand/contract migration once nothing depends on it being
-- present at all — tracked in docs/security-review.md.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- digest() below is pgcrypto's; gen_random_uuid() has been built into core
-- Postgres since 13 and needs no extension, so this is the only reason this
-- migration chain needs one.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS api_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          text NOT NULL DEFAULT '',
    key_hash      bytea NOT NULL,
    prefix        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz,
    CONSTRAINT api_keys_key_hash_unique UNIQUE (key_hash)
);

-- The hot path: hash the incoming header and look this row up directly.
-- key_hash's UNIQUE constraint above already gives it an index; this one
-- covers "list my live keys" without a table scan.
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys (user_id, created_at DESC)
    WHERE revoked_at IS NULL;

-- A key belongs to a person, never a team: a team is a namespace, not a
-- credential holder (see 0019_add_teams.sql), and this is the database-level
-- half of that rule surviving a rolled-back binary or hand-written SQL, the
-- same shape teams_have_no_key already uses for users.api_key.
CREATE OR REPLACE FUNCTION api_keys_person_only() RETURNS trigger AS $$
BEGIN
    IF (SELECT kind FROM users WHERE id = NEW.user_id) <> 'person' THEN
        RAISE EXCEPTION 'api_keys.user_id must be a person, not a team';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS api_keys_person_only ON api_keys;
CREATE TRIGGER api_keys_person_only
    BEFORE INSERT ON api_keys
    FOR EACH ROW EXECUTE FUNCTION api_keys_person_only();

-- Backfill: one row per account that still holds a plaintext key, so nothing
-- that already authenticates with it is cut off the moment this migration
-- lands. Named "legacy" so it reads, in the dashboard's key list, as
-- something to replace rather than something newly minted. Skips teams —
-- the teams_have_no_key trigger already guarantees none has an api_key, but
-- this is defensive, matching the migration's own trigger above.
INSERT INTO api_keys (user_id, name, key_hash, prefix, created_at)
SELECT id,
       'legacy (from registration)',
       digest(api_key, 'sha256'),
       left(encode(digest(api_key, 'sha256'), 'hex'), 8),
       created_at
FROM users
WHERE api_key IS NOT NULL
  AND kind = 'person'
ON CONFLICT (key_hash) DO NOTHING;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE api_keys TO simplehost_app;

COMMIT;
