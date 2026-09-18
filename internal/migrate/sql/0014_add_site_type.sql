-- Site type for grouping the public showcase.
--
-- Both columns are nullable and stay that way: a site that has not been
-- classified is a normal, permanent state, not a fault. It renders as
-- "Unsorted" and the page works exactly as it did before.
--
-- site_type_version fences the write against the version it was derived from.
-- Classification is asynchronous, so two passes for different versions of the
-- same site can finish out of order; the writer only moves the type forward.
-- The key is the immutable site id, so deleting and recreating a site with the
-- same name cannot inherit a stale label.
--
-- Nothing here touches updated_at. That column orders the showcase's "Recently
-- updated" view, and a background classifier that bumped it would silently
-- reorder the page.

ALTER TABLE sites ADD COLUMN IF NOT EXISTS site_type TEXT;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS site_type_version BIGINT;

-- Partial: the showcase only ever reads classified public sites, and the
-- backfill only ever looks for unclassified ones.
CREATE INDEX IF NOT EXISTS sites_site_type_idx
    ON sites (site_type)
    WHERE site_type IS NOT NULL;
