-- Not backward-compatible: it only adds nullable columns, but a binary that
-- does not know them would let an owner widen a site an admin restricted
-- (0058 corrects the record on databases migrated before this was said).
--
-- The last admin decision about who can open a site, so its owner can see
-- it: a network-access request declined, network access revoked, or the site
-- restricted to only its owner (or team) by an admin. access_decision_reason
-- is the admin's optional one-line note (required for a restriction);
-- access_decision_previous is the level a restriction replaced, which lifting
-- the restriction restores. A declined or revoked decision stays until the
-- owner's next network-access request; a restriction until an admin lifts
-- it.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE sites ADD COLUMN IF NOT EXISTS access_decision text;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS access_decision_at timestamptz;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS access_decision_reason text;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS access_decision_previous text;

ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_access_decision_check;
ALTER TABLE sites ADD CONSTRAINT sites_access_decision_check CHECK (
    access_decision IS NULL OR access_decision IN ('declined', 'revoked', 'restricted')
) NOT VALID;

COMMIT;
