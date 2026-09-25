-- Add sites.state_version for optimistic concurrency on per-site state
-- — every state save bumps it: the legacy unconditional PUT increments it
-- blindly; the versioned endpoint's CAS UPDATE requires it to match.
--
-- ORDERING: apply this BEFORE deploying the binary that references the
-- column — the legacy UPDATE in UpdateSiteState mentions state_version, so
-- a new binary against an old schema would fail every legacy state save.
ALTER TABLE sites ADD COLUMN state_version BIGINT NOT NULL DEFAULT 0;
