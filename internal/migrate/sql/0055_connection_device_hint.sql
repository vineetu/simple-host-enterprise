-- simple-host: backward-compatible
-- Marked compatible because it only adds two columns with a constant
-- default that older code ignores: an older binary lists connected apps
-- without the hint.
--
-- device_hint is a short summary of the browser that pressed Allow on the
-- consent page ("Chrome on macOS"), so a person with two connections of the
-- same app can tell them apart on the sessions page. It is worked out from
-- the User-Agent at that moment and stored as the summary only: never the
-- raw header, never an IP address. It is written on the authorization code
-- and copied to the grant when the code is redeemed.
--
-- Lock duration: two ADD COLUMNs with a constant default, catalog-only
-- changes on Postgres 11+ (no table rewrite).

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE oauth_codes
    ADD COLUMN IF NOT EXISTS device_hint text NOT NULL DEFAULT '';
ALTER TABLE oauth_grants
    ADD COLUMN IF NOT EXISTS device_hint text NOT NULL DEFAULT '';

COMMIT;
