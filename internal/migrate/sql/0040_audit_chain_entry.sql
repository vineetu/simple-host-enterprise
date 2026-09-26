-- simple-host: backward-compatible
-- Marked compatible because it only adds one read-only function that no
-- older code calls: a rollback runs unchanged against the schema this leaves.
--
-- The chain's seq and hash for one audit_events row, so the server can put
-- them on the event's SIEM line (see docs/configuration.md). The application
-- role still has no privilege on audit_chain itself; this SECURITY DEFINER
-- function lets it read exactly the (seq, hash) of an event it names by
-- (id, at), nothing else. Called inside the inserting transaction, it sees
-- the chain row the trigger just wrote.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE OR REPLACE FUNCTION audit_chain_entry(p_event_id bigint, p_event_at timestamptz)
RETURNS TABLE (seq bigint, hash bytea)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT c.seq, c.hash FROM audit_chain c
    WHERE c.event_at = p_event_at AND c.event_id = p_event_id
$$;

REVOKE ALL ON FUNCTION audit_chain_entry(bigint, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_chain_entry(bigint, timestamptz) TO simplehost_app;

COMMIT;
