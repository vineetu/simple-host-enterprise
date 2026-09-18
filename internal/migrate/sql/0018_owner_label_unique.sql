-- Make the owner label unique across accounts.
--
-- Every account is addressed as "<label>.<base>", where label is the username
-- lowercased with every '.' replaced by '-' (ownerLabel in
-- internal/handler/host.go). The server only ever compares username -> label
-- and never resolves a label back to a username, so two accounts whose names
-- map to one label ("alice.b" and "alice-b") would each pass the other's host
-- checks. Nothing at read time can tell them apart; the only place to prevent
-- it is at write time, and that is this index.
--
-- The index expression must stay byte-for-byte what ownerLabel computes. If
-- the Go rule changes, this index is wrong and must be rebuilt in a later
-- migration; the comment on ownerLabel points back here for that reason.
--
-- A pre-existing duplicate is a hard stop. Two accounts already sharing a
-- label cannot both keep their hostname, and choosing which one loses it is a
-- decision for a person, not this file. The DO block below names them and
-- aborts so nothing is created until they are resolved by hand.
--
-- Re-runnable. The running binary does not depend on the index existing (it
-- only maps a Postgres unique violation on it to a 409), so this can be
-- applied before or after the deploy. It must be applied before any subdomain
-- DNS record is published: until then a colliding account can still be created
-- and the host checks would accept it.

BEGIN;

-- Fail fast rather than queue behind a long transaction while holding a lock
-- on users, which every authenticated request reads.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

DO $$
DECLARE
    duplicates text;
BEGIN
    SELECT string_agg(label || ' (' || names || ')', ', ' ORDER BY label)
    INTO duplicates
    FROM (
        SELECT lower(replace(username, '.', '-')) AS label,
               string_agg(username, ', ' ORDER BY username) AS names
        FROM users
        GROUP BY lower(replace(username, '.', '-'))
        HAVING count(*) > 1
    ) AS collisions;

    IF duplicates IS NOT NULL THEN
        RAISE EXCEPTION 'accounts share an owner label; resolve by hand before applying: %', duplicates;
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS users_owner_label_idx
    ON users ((lower(replace(username, '.', '-'))));

COMMIT;
