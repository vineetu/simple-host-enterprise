-- Audit events and access log.
--
-- Both tables are partitioned by month on their timestamp column so
-- retention is a partition drop (`simple-host prune`, cmd/server/subcommands.go)
-- rather than a DELETE: the application role gets only INSERT and SELECT
-- on audit_events and access_log, never DELETE, so the running server
-- process is structurally unable to erase its own trail. Dropping a
-- partition is DDL, which only the owning role (this migration's role) can
-- do, which is why pruning runs as its own subcommand under the owner
-- credential and never inside the request path.
--
-- Every partitioned table here also gets one DEFAULT partition, so an
-- INSERT never fails with "no partition of relation found for row" if
-- audit_ensure_partitions below has fallen behind the calendar, and so the
-- team_audit fold a few statements down has somewhere to land regardless
-- of how old its rows are, without this migration needing to compute that
-- table's full historical date range. A default partition is not part of
-- the date-bounded retention sweep `simple-host prune` runs; see
-- internal/audit/prune.go.
--
-- state_write coalescing ("one row per (actor, site, five-minute window)")
-- is the one place the application role writes to an existing
-- row instead of only inserting one, so it is carved out narrowly: a
-- SECURITY DEFINER function that can only ever bump detail.count on a
-- state_write row it just upserted, never touch any other column or action.
-- The table grant itself stays INSERT/SELECT only.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS audit_events (
    id                 bigserial NOT NULL,
    at                 timestamptz NOT NULL DEFAULT now(),
    request_id         text,
    actor_id           uuid REFERENCES users(id) ON DELETE SET NULL,
    actor_kind         text NOT NULL DEFAULT 'person',
    key_id             uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    action             text NOT NULL,
    owner_id           uuid REFERENCES users(id) ON DELETE SET NULL,
    site_id            uuid REFERENCES sites(id) ON DELETE SET NULL,
    team_id            uuid REFERENCES users(id) ON DELETE SET NULL,
    via_site_label     text,
    via_site_name      text,
    via_site_observed  boolean NOT NULL DEFAULT false,
    ip                 inet,
    user_agent         text,
    detail             jsonb NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT audit_events_actor_kind_check CHECK (actor_kind IN ('person', 'key', 'system')),
    -- The partition key (at) must be part of every unique constraint on a
    -- partitioned table, including the primary key; bigserial id alone is
    -- already unique in practice (one sequence, never reused across
    -- partitions), but Postgres requires the pairing regardless.
    PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);

CREATE TABLE IF NOT EXISTS audit_events_default PARTITION OF audit_events DEFAULT;

CREATE INDEX IF NOT EXISTS audit_events_at_idx ON audit_events (at DESC);
CREATE INDEX IF NOT EXISTS audit_events_owner_at_idx ON audit_events (owner_id, at DESC);
CREATE INDEX IF NOT EXISTS audit_events_site_at_idx ON audit_events (site_id, at DESC);
CREATE INDEX IF NOT EXISTS audit_events_actor_at_idx ON audit_events (actor_id, at DESC);

-- The coalescing arbiter: one live state_write row per (site, actor,
-- window). Partial so it costs nothing on every other action. Also
-- excludes a NULL site_id or actor_id: Postgres never considers one NULL
-- equal to another, so a unique index alone would let every state_write
-- missing either column insert its own row forever instead of coalescing
-- (the "one row per window" rule) — a real bug this migration
-- originally shipped with (a review finding). Every real
-- state_write has both columns; audit_bump_state_write below also refuses
-- a NULL in either, so this predicate should never actually exclude a row
-- in practice, but the index stays correct even if some future caller
-- reaches the table directly.
CREATE UNIQUE INDEX IF NOT EXISTS audit_events_state_write_window_idx
    ON audit_events (site_id, actor_id, at)
    WHERE action = 'state_write' AND site_id IS NOT NULL AND actor_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS access_log (
    id          bigserial NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    user_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    session_id  uuid REFERENCES sessions(id) ON DELETE SET NULL,
    owner_label text NOT NULL,
    site_name   text NOT NULL,
    path        text NOT NULL,
    method      text NOT NULL,
    status      smallint NOT NULL,
    bytes       bigint NOT NULL DEFAULT 0,
    ip          inet,
    user_agent  text,
    client_kind text NOT NULL DEFAULT 'human',
    PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);

CREATE TABLE IF NOT EXISTS access_log_default PARTITION OF access_log DEFAULT;

CREATE INDEX IF NOT EXISTS access_log_owner_site_at_idx ON access_log (owner_label, site_name, at DESC);
CREATE INDEX IF NOT EXISTS access_log_user_at_idx ON access_log (user_id, at DESC);

-- Rolling partition management. Owner-role only (creating a partition is
-- DDL); the application role never calls this. Run once below to seed the
-- current and next two months, and again every month from `simple-host
-- prune` (internal/audit/prune.go) so serving never outruns the calendar.
-- Idempotent: to_regclass short-circuits a month that already has its
-- partition, so calling it twice in the same month, or replaying this
-- migration's own bootstrap call, does nothing the second time.
CREATE OR REPLACE FUNCTION audit_ensure_partitions(months_ahead int DEFAULT 2)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    i           int;
    month_start date;
    month_end   date;
    bound_from  text;
    bound_to    text;
    part_name   text;
BEGIN
    IF months_ahead < 0 THEN
        RAISE EXCEPTION 'audit_ensure_partitions: months_ahead must not be negative';
    END IF;
    FOR i IN 0..months_ahead LOOP
        -- Computed in UTC wall-clock terms (date arithmetic has no time
        -- zone to get wrong), then the boundary is written back out as an
        -- explicit UTC instant so the partition bound means the same thing
        -- regardless of the connecting session's time zone setting.
        month_start := (date_trunc('month', timezone('UTC', now()))::date + (i || ' months')::interval)::date;
        month_end   := (month_start + interval '1 month')::date;
        bound_from  := to_char(month_start, 'YYYY-MM-DD') || ' 00:00:00+00';
        bound_to    := to_char(month_end,   'YYYY-MM-DD') || ' 00:00:00+00';

        part_name := 'audit_events_p' || to_char(month_start, 'YYYY_MM');
        IF to_regclass(part_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF audit_events FOR VALUES FROM (%L) TO (%L)',
                part_name, bound_from, bound_to
            );
        END IF;

        part_name := 'access_log_p' || to_char(month_start, 'YYYY_MM');
        IF to_regclass(part_name) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF access_log FOR VALUES FROM (%L) TO (%L)',
                part_name, bound_from, bound_to
            );
        END IF;
    END LOOP;
END;
$$;

SELECT audit_ensure_partitions(2);

-- The one narrow write the application role gets beyond INSERT/SELECT:
-- bump an existing state_write row's count, or create the window's first
-- one. SECURITY DEFINER so the grant below can hand out exactly this
-- upsert without a general UPDATE on the table; SET search_path pins name
-- resolution so a caller cannot hijack it by manipulating their own
-- session's search_path.
--
-- p_actor_id and p_site_id are required (raised, not silently coerced):
-- they are the arbiter's key alongside p_window_start, and Postgres never
-- considers one NULL equal to another, so a NULL in either would insert a
-- fresh, never-coalescing row every call instead of upserting one per
-- window (a review finding — internal/audit's DBRecorder and
-- internal/db's BumpStateWrite both already refuse this before it reaches
-- here; this is the same rule enforced again at the one place every path
-- to this table must pass through).
CREATE OR REPLACE FUNCTION audit_bump_state_write(
    p_window_start  timestamptz,
    p_actor_id      uuid,
    p_owner_id      uuid,
    p_site_id       uuid,
    p_request_id    text,
    p_actor_kind    text,
    p_key_id        uuid,
    p_ip            inet,
    p_user_agent    text
)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
BEGIN
    IF p_actor_id IS NULL OR p_site_id IS NULL THEN
        RAISE EXCEPTION 'audit_bump_state_write: actor_id and site_id are both required';
    END IF;

    INSERT INTO audit_events (at, request_id, actor_id, actor_kind, key_id, action, owner_id, site_id, ip, user_agent, detail)
    VALUES (p_window_start, p_request_id, p_actor_id, COALESCE(NULLIF(p_actor_kind, ''), 'person'), p_key_id, 'state_write', p_owner_id, p_site_id, p_ip, p_user_agent, jsonb_build_object('count', 1))
    ON CONFLICT (site_id, actor_id, at) WHERE action = 'state_write' AND site_id IS NOT NULL AND actor_id IS NOT NULL
    DO UPDATE SET
        detail     = jsonb_set(audit_events.detail, '{count}', to_jsonb(COALESCE((audit_events.detail->>'count')::int, 0) + 1)),
        request_id = EXCLUDED.request_id,
        ip         = EXCLUDED.ip,
        user_agent = EXCLUDED.user_agent;
END;
$$;

-- team_audit fold-in: team_audit is folded into this table, and migration
-- 0019's table is migrated and dropped; the drop itself is a separate,
-- later, one-way migration that drops team_audit one release after 0025.
-- This migration does only the fold, deliberately not the drop:
-- internal/db/teams.go still reads and writes team_audit as of this
-- migration (team work is in flight in parallel with this one), so
-- dropping the table here would take down a live code path this package
-- does not own. Guarded by existence so re-running this file after a
-- prior attempt that got this far (and then failed later in the same
-- transaction, which rolls the fold back too) does not double-insert. The
-- actual DROP TABLE belongs to a follow-up migration once nothing
-- references team_audit any more -- see docs/security-review.md.
--
-- subject_id has no column of its own in audit_events; it moves into
-- detail, alongside the free-text note team_audit already used member
-- add/remove for.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'team_audit')
       AND NOT EXISTS (
           SELECT 1 FROM audit_events
           WHERE action IN ('team_create', 'team_delete', 'member_add', 'member_remove')
             AND detail ? 'folded_from_team_audit'
       )
    THEN
        INSERT INTO audit_events (at, actor_id, actor_kind, action, team_id, detail)
        SELECT
            created_at,
            actor_id,
            'person',
            action,
            team_id,
            jsonb_strip_nulls(jsonb_build_object(
                'subject_id', subject_id,
                'note', NULLIF(detail, ''),
                'folded_from_team_audit', true
            ))
        FROM team_audit;
    END IF;
END
$$;

-- Least-privilege grants: INSERT and SELECT only, no UPDATE, no DELETE. A
-- bigserial id, unlike every uuid/gen_random_uuid() primary key
-- elsewhere in this schema, needs its backing sequence's privileges granted
-- explicitly — table-level INSERT does not imply permission to call the
-- column default's nextval().
GRANT SELECT, INSERT ON TABLE audit_events TO simplehost_app;
GRANT SELECT, INSERT ON TABLE access_log TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE audit_events_id_seq TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE access_log_id_seq TO simplehost_app;
GRANT EXECUTE ON FUNCTION audit_bump_state_write(timestamptz, uuid, uuid, uuid, text, text, uuid, inet, text) TO simplehost_app;

COMMIT;
