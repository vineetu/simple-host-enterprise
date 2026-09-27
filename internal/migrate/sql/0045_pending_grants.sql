-- simple-host: backward-compatible
-- Marked compatible because it only adds tables no older code reads: rolled
-- back, pending grants are simply not shown or converted until the newer
-- binary returns.
--
-- A site or team shared by company email with someone who has not signed in
-- yet. Each row is keyed by the lower-cased address and becomes the real
-- grant (a site_viewers row or a team_members row) at that person's first
-- sign-in with a verified matching email, when the row is deleted. Rows go
-- with their site or team.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS pending_site_viewers (
    site_id    uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    email      text NOT NULL CHECK (email = lower(email)),
    added_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, email)
);
CREATE INDEX IF NOT EXISTS pending_site_viewers_email_idx ON pending_site_viewers (email);

CREATE TABLE IF NOT EXISTS pending_team_members (
    team_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email      text NOT NULL CHECK (email = lower(email)),
    added_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, email)
);
CREATE INDEX IF NOT EXISTS pending_team_members_email_idx ON pending_team_members (email);

GRANT SELECT, INSERT, DELETE ON TABLE pending_site_viewers, pending_team_members TO simplehost_app;

COMMIT;
