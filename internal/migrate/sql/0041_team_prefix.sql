-- simple-host: backward-compatible
-- Marked compatible because it only renames rows: no table, column or index
-- changes, and an older binary serves a team called "team-sales" exactly as
-- it would any other team. Rolled back, the old "sales.<base>" address stops
-- redirecting; nothing else changes.
--
-- v1.3: every team name begins "team-", so a team can never take a name a
-- person signs in with. Older teams are renamed "team-<name>"; the server
-- redirects their old addresses for as long as no account takes the old
-- name (db.LegacyTeamName). A team already named "team-..." keeps its name.
--
-- Refuses to run if a renamed team would collide with an existing account's
-- address, or would be longer than one DNS label allows. The account must be
-- removed or the team deleted first; the error names them.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

DO $$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(t.username || ' -> ' || u.username, ', ')
    INTO clash
    FROM users t
    JOIN users u
      ON lower(replace(u.username, '.', '-')) = 'team-' || lower(replace(t.username, '.', '-'))
    WHERE t.kind = 'team' AND t.username NOT LIKE 'team-%';
    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'renaming teams to team-<name> would collide with existing accounts: %', clash;
    END IF;
    SELECT string_agg(username, ', ') INTO clash
    FROM users WHERE kind = 'team' AND username NOT LIKE 'team-%' AND length(username) > 58;
    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'team names too long to take the team- prefix (58 characters at most): %', clash;
    END IF;
END $$;

UPDATE site_search_documents d
SET owner_name = 'team-' || u.username
FROM sites s
JOIN users u ON u.id = s.user_id
WHERE d.site_id = s.id AND u.kind = 'team' AND u.username NOT LIKE 'team-%';

UPDATE users
SET username = 'team-' || username
WHERE kind = 'team' AND username NOT LIKE 'team-%';

COMMIT;
