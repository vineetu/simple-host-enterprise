-- simple-host: backward-compatible
-- Marked compatible because it only adds a column with a constant default
-- that older code ignores: an older binary writes '' into it.
--
-- access_log.referrer_domain is the host of the page that linked to a
-- visit, and only the host: never the path or query of the Referer, which
-- can carry anything. '' when there was none, when it came from the same
-- site (moving between its own pages), or when it was not an http(s) URL.
-- Another page on this install (another site, an owner page, search or the
-- showcase) is recorded as '*.<base>' or '<base>', so the name of a site an
-- owner may not open never reaches them through their referrer list. The
-- dashboard's Visitors view counts visits by it ("where visitors came
-- from"), under ACCESS_LOG_VISIBILITY like every other count.
--
-- Lock duration: one ADD COLUMN with a constant default on the partitioned
-- table, catalog-only on every partition (Postgres 11+ fast default, no
-- rewrite).

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE access_log
    ADD COLUMN IF NOT EXISTS referrer_domain text NOT NULL DEFAULT '';

COMMIT;
