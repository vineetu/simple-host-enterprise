-- Restricted-site viewing and the session hand-off (design.md 5.2a, 6.1, 7.2).
--
-- site_viewers is the per-site allow-list: no rows means "any signed-in
-- person may view," one or more rows means only those principals (a person
-- or a team, both rows in users) may. Restricting a site (adding its first
-- row) is also what moves its address from "<owner>.<base>/<site>/" to its
-- own flat label "<owner>--<site>.<base>" (handler.HostModel); this table
-- has no column for that, the application derives it from row presence
-- alone, so the two can never disagree.
--
-- state_write_mode governs the site-facing API's writerAllowed rule
-- (design.md 7.3, built in Phase 3): 'anyone' is today's behaviour, minus
-- anonymous access, and 'editors' restricts writes to the owner, an
-- owner-team member, or an editor. Added here, a phase early, because it is
-- one column on an existing table and the dashboard's viewer-list page is a
-- natural place to expose it alongside the viewer list itself.
--
-- handoff_codes backs the one-time hand-off (design.md 6.1): a code minted
-- by GET <base>/auth/handoff and redeemed exactly once by
-- <label>.<base>/auth/session, bound to the session, the target host, and a
-- hash of the nonce cookie set on that host. Single use is enforced by
-- redeemed_at; the 60-second window is enforced by created_at at redemption
-- time, not by a separate expiry column, so there is only one clock to get
-- right.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites ADD COLUMN IF NOT EXISTS state_write_mode text NOT NULL DEFAULT 'anyone';
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_state_write_mode_check;
ALTER TABLE sites ADD CONSTRAINT sites_state_write_mode_check
    CHECK (state_write_mode IN ('anyone', 'editors'));

CREATE TABLE IF NOT EXISTS site_viewers (
    site_id      uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, principal_id)
);

-- The existence check viewerAllowed and the restricted-address decision both
-- run first ("does this site have any viewers at all"); an index on site_id
-- alone (the primary key already starts with it) covers that and the
-- membership lookup in one.

CREATE TABLE IF NOT EXISTS handoff_codes (
    code        text PRIMARY KEY,
    session_id  uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    target_host text NOT NULL,
    nonce_hash  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    redeemed_at timestamptz
);

-- Codes outlive their 60-second usefulness by design (an audit trail of the
-- attempt), so nothing here deletes them; a later retention pass can prune
-- rows older than a day the same way audit_events will be pruned (Phase 4).

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE site_viewers TO simplehost_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE handoff_codes TO simplehost_app;

COMMIT;
