-- Per-site access levels, network-access requests, saved-data history, and
-- the end of per-site editor grants.
--
-- sites.access is the one answer to "who can open this site":
--   only_me   the owner, or for a team site the team's members (new default)
--   specific  plus named viewers (site_viewers); served on its own host
--   company   any signed-in person with the link
--   listed    company, and shown in the showcase and search
--   network   anyone who can reach the server, no sign-in; set only when an
--             admin approves the request recorded in network_requested_*
-- sites.public stays, kept equal to access IN ('listed', 'network') by the
-- only code that writes access, so search and the showcase read it unchanged.
--
-- Existing sites keep what they had: a site with viewers is 'specific', a
-- public one 'listed', anything else 'company', so no shared link breaks.
--
-- Not backward-compatible: it drops site_collaborators (editor grants) and
-- sites.state_write_mode, which older binaries read.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

ALTER TABLE sites ADD COLUMN IF NOT EXISTS access text;
UPDATE sites s SET access = CASE
    WHEN EXISTS (SELECT 1 FROM site_viewers sv WHERE sv.site_id = s.id) THEN 'specific'
    WHEN s.public THEN 'listed'
    ELSE 'company'
END
WHERE access IS NULL;
UPDATE sites SET public = (access IN ('listed', 'network'));
ALTER TABLE sites ALTER COLUMN access SET DEFAULT 'only_me';
ALTER TABLE sites ALTER COLUMN access SET NOT NULL;
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_access_check;
ALTER TABLE sites ADD CONSTRAINT sites_access_check
    CHECK (access IN ('only_me', 'specific', 'company', 'listed', 'network'));

ALTER TABLE sites ADD COLUMN IF NOT EXISTS network_requested_at timestamptz;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS network_requested_by uuid REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS network_request_reason text;
CREATE INDEX IF NOT EXISTS sites_network_requested_idx ON sites (network_requested_at)
    WHERE network_requested_at IS NOT NULL;

-- The last 20 saved states of each site, newest first by id. Every state
-- write inserts one row and prunes beyond 20 in the same transaction.
CREATE TABLE IF NOT EXISTS site_state_history (
    id            bigserial PRIMARY KEY,
    site_id       uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    state_version bigint NOT NULL,
    state         jsonb NOT NULL,
    written_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS site_state_history_site_idx ON site_state_history (site_id, id DESC);

DROP TABLE IF EXISTS site_collaborators;
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_state_write_mode_check;
ALTER TABLE sites DROP COLUMN IF EXISTS state_write_mode;

GRANT SELECT, INSERT, DELETE ON TABLE site_state_history TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE site_state_history_id_seq TO simplehost_app;

COMMIT;
