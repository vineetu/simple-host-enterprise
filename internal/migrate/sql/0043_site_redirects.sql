-- simple-host: backward-compatible
-- Marked compatible because it only adds a table no older code reads. Rolled
-- back, a moved or renamed site's old address stops redirecting; nothing else
-- changes.
--
-- One row per address a site used to have: a site handed to another person
-- or team, or renamed, keeps its old "<site part>.<owner label>.<base>"
-- address as a redirect to wherever the site is now. The row follows the
-- site (site_id), so a site moved twice redirects from both old addresses to
-- the current one, and deleting the site drops its redirects. An address a
-- live site holds is always served, never redirected, so reusing the name
-- takes the address back. The owner-hosts reconciler keeps a certificate
-- for every owner label here, so the old address still answers over TLS.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS site_redirects (
    owner_label text NOT NULL,
    site_part   text NOT NULL,
    site_id     uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_label, site_part)
);

CREATE INDEX IF NOT EXISTS site_redirects_site_idx ON site_redirects (site_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE site_redirects TO simplehost_app;

COMMIT;
