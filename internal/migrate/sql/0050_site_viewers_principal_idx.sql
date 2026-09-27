-- simple-host: backward-compatible
-- Marked compatible because it only adds an index no older code depends on.
--
-- "Shared with me" lists the sites a person, or a team they are in, is a
-- named viewer of: a lookup by principal. The primary key starts with
-- site_id, so without this the list scans every viewer row. site_viewers is
-- small (at most 50 rows a site), so the brief write lock is bounded.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

CREATE INDEX IF NOT EXISTS site_viewers_principal_idx ON site_viewers (principal_id);

COMMIT;
