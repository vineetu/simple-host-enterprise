-- Add sites.public flag. Existing sites are backfilled to true (grandfathered
-- visible). New sites default to false (unlisted) until the owner opts in via
-- POST /api/sites/{sitename}/visibility.
--
-- Wrapped in BEGIN/COMMIT so the ADD COLUMN + UPDATE backfill are atomic. No
-- window where a query could see existing sites as public=false.
BEGIN;

ALTER TABLE sites ADD COLUMN public BOOLEAN NOT NULL DEFAULT false;

UPDATE sites SET public = true;

ALTER TABLE sites ALTER COLUMN public SET DEFAULT false;

COMMIT;
