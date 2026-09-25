-- API keys expire. A key is for CI and other automation (people and their
-- agents sign in through OIDC), and a credential that never expires
-- outlives the job it was minted for. New keys get 90 days by default and
-- at most API_KEY_MAX_DAYS (365); every key that already exists gets 90
-- days from this migration, so nothing is cut off the moment it lands.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at timestamptz;
UPDATE api_keys SET expires_at = now() + interval '90 days' WHERE expires_at IS NULL;
ALTER TABLE api_keys ALTER COLUMN expires_at SET NOT NULL;

COMMIT;
