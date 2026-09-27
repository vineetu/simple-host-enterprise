-- simple-host: backward-compatible
-- Marked compatible because it only adds a column and a table older code
-- ignores: an older binary goes back to judging idle sites by visits in the
-- analytics, and its export links work again until they expire.
--
-- sites.last_used_at is when the site was last opened (by anyone, its owner
-- and team included; not by previews or bots), its saved data read or
-- written, or a version deployed. The server bumps it at most hourly per
-- site; the idle cleanup (IDLE_CLEANUP_DAYS) judges a site by it. Existing
-- sites start at the time this migration runs, because nobody knows when
-- they were last opened: the cleanup waits a full IDLE_CLEANUP_DAYS from
-- now before calling any of them idle, rather than treating missing data as
-- disuse.
--
-- site_export_links_used holds each whole-site download link once its
-- download has started, so a link works once. Rows are dropped once the
-- link has expired anyway (10 minutes). It holds the link's signature, never
-- who used it.
--
-- Lock duration: one ADD COLUMN with a default Postgres 11+ evaluates once
-- and records in the catalog (no table rewrite), and one new empty table.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS last_used_at timestamptz NOT NULL DEFAULT now();

CREATE TABLE IF NOT EXISTS site_export_links_used (
    signature  text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

GRANT SELECT, INSERT, DELETE ON TABLE site_export_links_used TO simplehost_app;

COMMIT;
