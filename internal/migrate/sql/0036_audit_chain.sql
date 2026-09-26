-- simple-host: backward-compatible
-- Marked compatible because it only adds two tables, two functions and an
-- AFTER INSERT trigger. The trigger is self-contained: a v1.1.3 server's
-- INSERT into audit_events and its audit_bump_state_write call work
-- unchanged, and are chained like any other row. (A v1.1.3 `prune` does not
-- trim the chain; run `simple-host prune` from this release or later.)
--
-- Hash chain over audit_events: tamper evidence.
--
-- Every row inserted into audit_events from now on gets one row in
-- audit_chain: hash = sha256(prev_hash || audit_event_canonical(row)), where
-- prev_hash is the previous chain row's hash (32 zero bytes for the first).
-- `simple-host audit-verify` walks the chain and recomputes every hash with
-- the same SQL function, so a changed or deleted event, or a deleted chain
-- row, breaks the chain at the seq where it happened.
--
-- How the chain stays one chain across replicas: the trigger takes a row
-- lock on the single audit_chain_head row (SELECT ... FOR UPDATE) before it
-- reads the last hash, and writes the new head in the same transaction, so
-- two servers inserting at once are serialized there and can never both
-- extend the same prev_hash. The lock is held until the inserting
-- transaction commits. Audited mutations are low-volume, and the one
-- frequent action, state_write, takes the lock only on the first write of
-- each (site, person, five-minute) window: the later writes in the window
-- are an ON CONFLICT DO UPDATE, for which an AFTER INSERT row trigger does
-- not fire. Every server code path records its audit row last in its
-- transaction, so the head lock is the last lock a transaction takes and
-- cannot be held while waiting for another row lock.
--
-- What is and is not covered:
--   * Rows that existed before this migration are not chained.
--   * For state_write, detail.count (the only thing a later write in the
--     window changes) is left out of the canonical form, so the count
--     itself is not tamper-evident; the row's existence, time, actor, site
--     and addresses are.
--   * access_log is not chained: it is written in batches by an async
--     writer at page-view volume, and a global lock per view would
--     serialize every page view across replicas.
--   * `simple-host prune` drops old partitions and then deletes the chain
--     rows for the dropped events (a prefix of the chain); verification
--     starts at the first remaining row and trusts its prev_hash as the
--     anchor.
--   * The database owner can rewrite both tables and recompute the chain.
--     The chain proves nothing against the owner on its own; keep the
--     head hash audit-verify prints (or the SIEM stream) outside the
--     database to compare against.
--
-- The application role gets no privilege on either table: the trigger
-- function is SECURITY DEFINER (owned by this migration's role), so the
-- only way a row reaches audit_chain is an INSERT into audit_events.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS audit_chain (
    seq        bigint PRIMARY KEY,
    event_id   bigint NOT NULL,
    event_at   timestamptz NOT NULL,
    prev_hash  bytea NOT NULL,
    hash       bytea NOT NULL
);

CREATE INDEX IF NOT EXISTS audit_chain_event_at_idx ON audit_chain (event_at);

-- Exactly one row: the last seq and hash. The row lock on it is the chain's
-- serialization point.
CREATE TABLE IF NOT EXISTS audit_chain_head (
    id    boolean PRIMARY KEY DEFAULT true CHECK (id),
    seq   bigint NOT NULL,
    hash  bytea NOT NULL
);

INSERT INTO audit_chain_head (id, seq, hash)
VALUES (true, 0, decode(repeat('00', 32), 'hex'))
ON CONFLICT (id) DO NOTHING;

-- The one canonical form of an event, used by the trigger and by
-- verification. Scalar arguments rather than the row type, because inside a
-- trigger on a partitioned table NEW has the partition's row type. jsonb
-- output is deterministic (keys sorted, no insignificant whitespace), and
-- the time is rendered in UTC so the session's TimeZone cannot change it.
CREATE OR REPLACE FUNCTION audit_event_canonical(
    p_id                bigint,
    p_at                timestamptz,
    p_request_id        text,
    p_actor_id          uuid,
    p_actor_kind        text,
    p_key_id            uuid,
    p_action            text,
    p_owner_id          uuid,
    p_site_id           uuid,
    p_team_id           uuid,
    p_via_site_label    text,
    p_via_site_name     text,
    p_via_site_observed boolean,
    p_ip                inet,
    p_user_agent        text,
    p_detail            jsonb
)
RETURNS text
LANGUAGE sql
STABLE
SET search_path = public
AS $$
    SELECT jsonb_build_object(
        'id', p_id,
        'at', to_char(p_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'request_id', p_request_id,
        'actor_id', p_actor_id,
        'actor_kind', p_actor_kind,
        'key_id', p_key_id,
        'action', p_action,
        'owner_id', p_owner_id,
        'site_id', p_site_id,
        'team_id', p_team_id,
        'via_site_label', p_via_site_label,
        'via_site_name', p_via_site_name,
        'via_site_observed', p_via_site_observed,
        'ip', p_ip::text,
        'user_agent', p_user_agent,
        'detail', CASE WHEN p_action = 'state_write' THEN p_detail - 'count' ELSE p_detail END
    )::text
$$;

CREATE OR REPLACE FUNCTION audit_chain_append() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_seq  bigint;
    v_prev bytea;
    v_hash bytea;
BEGIN
    SELECT seq, hash INTO v_seq, v_prev FROM audit_chain_head WHERE id FOR UPDATE;
    v_seq := v_seq + 1;
    v_hash := sha256(v_prev || convert_to(audit_event_canonical(
        NEW.id, NEW.at, NEW.request_id, NEW.actor_id, NEW.actor_kind, NEW.key_id,
        NEW.action, NEW.owner_id, NEW.site_id, NEW.team_id, NEW.via_site_label,
        NEW.via_site_name, NEW.via_site_observed, NEW.ip, NEW.user_agent, NEW.detail
    ), 'UTF8'));
    INSERT INTO audit_chain (seq, event_id, event_at, prev_hash, hash)
    VALUES (v_seq, NEW.id, NEW.at, v_prev, v_hash);
    UPDATE audit_chain_head SET seq = v_seq, hash = v_hash WHERE id;
    RETURN NULL;
END;
$$;

-- AFTER, not BEFORE: a BEFORE INSERT trigger also fires for a state_write
-- upsert that turns into an update, and would advance the chain for a row
-- that was never inserted. An AFTER INSERT row trigger fires only for rows
-- actually inserted, and sees the final `at` migration 0030's BEFORE
-- trigger set.
DROP TRIGGER IF EXISTS audit_events_chain ON audit_events;
CREATE TRIGGER audit_events_chain
    AFTER INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_chain_append();

REVOKE ALL ON TABLE audit_chain, audit_chain_head FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_chain_append() FROM PUBLIC;

COMMIT;
