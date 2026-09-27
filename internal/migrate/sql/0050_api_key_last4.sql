-- simple-host: backward-compatible
-- Marked compatible because it only adds a nullable column no older code
-- reads or writes.
--
-- The last four characters of the key itself, kept at mint so a person can
-- match a key they found (in a log, a commit, a paste) to its row, and so
-- an admin revoking a leaked key can confirm which one it was. Four
-- characters of 64 random hex grant nothing. Keys minted before this
-- column have none and are shown as "earlier key".

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS last4 text;

COMMIT;
