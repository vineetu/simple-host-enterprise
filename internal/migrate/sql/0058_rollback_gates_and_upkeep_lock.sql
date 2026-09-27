-- simple-host: backward-compatible
-- Marked compatible because it only changes recorded metadata, a grant, and
-- a function body with the same signature and effect; v1.7 runs unchanged.
--
-- 1. Migrations 0047 (admin restriction) and 0048 (erasure) were recorded
--    as backward-compatible, but a binary that does not know them does not
--    enforce what they record: it would let an owner widen a site an admin
--    restricted, and let an erased person register again. They are now
--    recorded as not compatible, so the startup gate refuses such a binary
--    against this database (every released binary without them is already
--    refused by 0044, so this only closes the gap for images built between).
-- 2. owner_label_lock kept the default EXECUTE for PUBLIC; like every other
--    function since 0046 it is now granted to the application role only.
-- 3. audit_ensure_partitions takes a transaction advisory lock before it
--    reads either default partition, so two upkeep runs (prune overlapping
--    another prune or a migrate) queue instead of deadlocking: each used to
--    hold its read lock on a default partition while waiting for the
--    other's parent lock.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';

UPDATE schema_migrations SET backward_compatible = false WHERE version IN (47, 48);

REVOKE ALL ON FUNCTION owner_label_lock(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION owner_label_lock(text) TO simplehost_app;

CREATE OR REPLACE FUNCTION audit_ensure_partitions(months_ahead int DEFAULT 2)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    month_start date;
BEGIN
    IF months_ahead < 0 THEN
        RAISE EXCEPTION 'audit_ensure_partitions: months_ahead must not be negative';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('simple-host audit partition upkeep'));
    FOR month_start IN
        SELECT (date_trunc('month', timezone('UTC', now()))::date + (i || ' months')::interval)::date
        FROM generate_series(0, months_ahead) AS i
        UNION
        SELECT date_trunc('month', timezone('UTC', at))::date FROM audit_events_default
        UNION
        SELECT date_trunc('month', timezone('UTC', at))::date FROM access_log_default
        ORDER BY 1
    LOOP
        PERFORM audit_ensure_month_partition('audit_events', month_start);
        PERFORM audit_ensure_month_partition('access_log', month_start);
    END LOOP;
END;
$$;

REVOKE ALL ON FUNCTION audit_ensure_partitions(int) FROM PUBLIC;

COMMIT;
