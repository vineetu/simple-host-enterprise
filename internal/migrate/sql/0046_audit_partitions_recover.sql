-- simple-host: backward-compatible
-- Marked compatible because it only replaces audit_ensure_partitions (same
-- signature, same effect when nothing is stranded), adds a helper function,
-- and creates empty monthly partitions ahead of time. Older code reads none
-- of it differently.
--
-- Partition upkeep that cannot wedge. Before this, audit_ensure_partitions
-- created a month's partition with CREATE TABLE ... PARTITION OF, which
-- Postgres refuses once the DEFAULT partition already holds rows for that
-- month. Rows land in the default partition whenever `simple-host prune`
-- has not run for longer than the months created ahead (it created two), and
-- from then on every prune failed at that first step, before dropping
-- anything: retention stopped for good, and recovering needed hand-written
-- SQL.
--
-- Now a month whose rows are stranded in the default partition gets its
-- partition built beside the table, the rows moved into it unchanged (same
-- id, same `at`, so the audit hash chain still verifies: a plain table has
-- no triggers, so nothing re-stamps or re-chains them), and it is then
-- attached. The default partition is locked for the move, so nothing new
-- lands in that month's range in between. Past months stranded there get
-- their partitions too, so prune can drop them on schedule. Twelve months
-- are created ahead (was two), and the prune CronJob runs daily (was
-- monthly), so a missed run is a non-event.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';

-- One month's partition of parent (audit_events or access_log), moving any
-- rows for that month out of the default partition first. Owner-role only,
-- like audit_ensure_partitions.
CREATE OR REPLACE FUNCTION audit_ensure_month_partition(parent text, month_start date)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    part_name    text := parent || '_p' || to_char(month_start, 'YYYY_MM');
    default_name text := parent || '_default';
    bound_from   text := to_char(month_start, 'YYYY-MM-DD') || ' 00:00:00+00';
    bound_to     text := to_char((month_start + interval '1 month')::date, 'YYYY-MM-DD') || ' 00:00:00+00';
    stranded     boolean := false;
BEGIN
    IF to_regclass(part_name) IS NOT NULL THEN
        RETURN;
    END IF;
    IF to_regclass(default_name) IS NOT NULL THEN
        EXECUTE format('LOCK TABLE %I IN ACCESS EXCLUSIVE MODE', default_name);
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I WHERE at >= $1::timestamptz AND at < $2::timestamptz)', default_name)
            INTO stranded USING bound_from, bound_to;
    END IF;
    IF NOT stranded THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            part_name, parent, bound_from, bound_to);
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE %I (LIKE %I INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', part_name, parent);
    EXECUTE format(
        'WITH moved AS (DELETE FROM %I WHERE at >= $1::timestamptz AND at < $2::timestamptz RETURNING *) INSERT INTO %I SELECT * FROM moved',
        default_name, part_name) USING bound_from, bound_to;
    EXECUTE format('ALTER TABLE %I ATTACH PARTITION %I FOR VALUES FROM (%L) TO (%L)',
        parent, part_name, bound_from, bound_to);
END;
$$;

-- The current month and months_ahead after it, plus every month that has
-- rows stranded in either default partition. Idempotent.
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

-- Owner-role only (migrate and prune connect as the owning role, which
-- always holds EXECUTE on its own functions): nothing for PUBLIC, and so
-- nothing for simplehost_app, as 0036 and 0040 do for theirs. SECURITY
-- INVOKER means a caller without table privileges could do nothing anyway;
-- this keeps the grants saying so.
REVOKE ALL ON FUNCTION audit_ensure_month_partition(text, date) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_ensure_partitions(int) FROM PUBLIC;

SELECT audit_ensure_partitions(12);

COMMIT;
