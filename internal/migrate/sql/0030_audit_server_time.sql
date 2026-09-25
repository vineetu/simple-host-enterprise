-- audit_events.at is the database's clock, never the caller's.
--
-- The application role could previously insert a row with any `at` it
-- liked (InsertAuditEvent sent the client's time.Now(), and
-- audit_bump_state_write took the window start as an argument), so a
-- compromised server could back-date or future-date its own trail. A
-- trigger now sets `at` on every insert: now() for ordinary rows, and the
-- five-minute window now() falls in for a coalesced state_write. A row that
-- asks for a different month than now() is refused outright by Postgres
-- (a BEFORE trigger may not move a row to another partition).
--
-- audit_bump_state_write keeps its signature so a server still running the
-- previous release keeps working during a rollout, but ignores
-- p_window_start, and a later write in the same window no longer replaces
-- the first write's request_id, ip and user_agent — it only bumps the count.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE OR REPLACE FUNCTION audit_events_server_time() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.action = 'state_write' THEN
        NEW.at := date_bin('5 minutes', now(), TIMESTAMPTZ '2000-01-01 00:00:00+00');
    ELSE
        NEW.at := now();
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS audit_events_server_time ON audit_events;
CREATE TRIGGER audit_events_server_time
    BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_server_time();

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
    VALUES (date_bin('5 minutes', now(), TIMESTAMPTZ '2000-01-01 00:00:00+00'), p_request_id, p_actor_id, COALESCE(NULLIF(p_actor_kind, ''), 'person'), p_key_id, 'state_write', p_owner_id, p_site_id, p_ip, p_user_agent, jsonb_build_object('count', 1))
    ON CONFLICT (site_id, actor_id, at) WHERE action = 'state_write' AND site_id IS NOT NULL AND actor_id IS NOT NULL
    DO UPDATE SET
        detail = jsonb_set(audit_events.detail, '{count}', to_jsonb(COALESCE((audit_events.detail->>'count')::int, 0) + 1));
END;
$$;

COMMIT;
