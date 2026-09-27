-- simple-host: backward-compatible
-- Marked compatible because it only re-creates existing functions with the
-- same signatures and bodies and narrows PUBLIC's privileges; no older code
-- creates temporary tables or objects in the public schema.
--
-- Security fix for every release since 0027. A SECURITY DEFINER function
-- runs as its owner, the migration role. These were declared with
-- `SET search_path = public`, which leaves pg_temp out of the list, and
-- Postgres then searches pg_temp *first* for tables. The application role
-- could create a temporary table named like one of the function's tables,
-- give it a trigger, and have that trigger run as the owner (reproduced
-- against access_log_erase_visitor): rewriting the audit chain, or worse on
-- a superuser owner.
--
-- Each function is re-created with search_path pg_catalog, public, pg_temp
-- (pg_temp last, where it can shadow nothing) and every table and function
-- it names schema-qualified. audit_event_canonical is not a definer function
-- and names no table, but gets the same search_path for uniformity.
--
-- Defence in depth, for the database this runs in only: PUBLIC loses
-- TEMPORARY on the database (the application never creates temporary
-- tables) and CREATE on the public schema (already the default from
-- Postgres 15; older clusters granted it). The schema revoke runs only when
-- this role owns the schema, after granting itself CREATE, so a managed
-- database where the migration role is not the schema owner keeps working.
BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE OR REPLACE FUNCTION public.audit_bump_state_write(
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
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_actor_id IS NULL OR p_site_id IS NULL THEN
        RAISE EXCEPTION 'audit_bump_state_write: actor_id and site_id are both required';
    END IF;

    INSERT INTO public.audit_events AS e (at, request_id, actor_id, actor_kind, key_id, action, owner_id, site_id, ip, user_agent, detail)
    VALUES (date_bin('5 minutes', now(), TIMESTAMPTZ '2000-01-01 00:00:00+00'), p_request_id, p_actor_id, COALESCE(NULLIF(p_actor_kind, ''), 'person'), p_key_id, 'state_write', p_owner_id, p_site_id, p_ip, p_user_agent, jsonb_build_object('count', 1))
    ON CONFLICT (site_id, actor_id, at) WHERE action = 'state_write' AND site_id IS NOT NULL AND actor_id IS NOT NULL
    DO UPDATE SET
        detail = jsonb_set(e.detail, '{count}', to_jsonb(COALESCE((e.detail->>'count')::int, 0) + 1));
END;
$$;

CREATE OR REPLACE FUNCTION public.audit_chain_append() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_seq  bigint;
    v_prev bytea;
    v_hash bytea;
BEGIN
    SELECT seq, hash INTO v_seq, v_prev FROM public.audit_chain_head WHERE id FOR UPDATE;
    v_seq := v_seq + 1;
    v_hash := sha256(v_prev || convert_to(public.audit_event_canonical(
        NEW.id, NEW.at, NEW.request_id, NEW.actor_id, NEW.actor_kind, NEW.key_id,
        NEW.action, NEW.owner_id, NEW.site_id, NEW.team_id, NEW.via_site_label,
        NEW.via_site_name, NEW.via_site_observed, NEW.ip, NEW.user_agent, NEW.detail
    ), 'UTF8'));
    INSERT INTO public.audit_chain (seq, event_id, event_at, prev_hash, hash)
    VALUES (v_seq, NEW.id, NEW.at, v_prev, v_hash);
    UPDATE public.audit_chain_head SET seq = v_seq, hash = v_hash WHERE id;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION public.audit_chain_entry(p_event_id bigint, p_event_at timestamptz)
RETURNS TABLE (seq bigint, hash bytea)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT c.seq, c.hash FROM public.audit_chain c
    WHERE c.event_at = p_event_at AND c.event_id = p_event_id
$$;

ALTER FUNCTION public.audit_event_canonical(bigint, timestamptz, text, uuid, text, uuid, text, uuid, uuid, uuid, text, text, boolean, inet, text, jsonb)
    SET search_path = pg_catalog, public, pg_temp;

-- CREATE OR REPLACE keeps existing grants; restated so a fresh chain and an
-- upgraded one end the same.
REVOKE ALL ON FUNCTION public.audit_bump_state_write(timestamptz, uuid, uuid, uuid, text, text, uuid, inet, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.audit_bump_state_write(timestamptz, uuid, uuid, uuid, text, text, uuid, inet, text) TO simplehost_app;
REVOKE ALL ON FUNCTION public.audit_chain_append() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.audit_chain_entry(bigint, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.audit_chain_entry(bigint, timestamptz) TO simplehost_app;

DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
    IF pg_has_role(current_user, (SELECT nspowner FROM pg_namespace WHERE nspname = 'public'), 'USAGE') THEN
        EXECUTE format('GRANT CREATE ON SCHEMA public TO %I', current_user);
        REVOKE CREATE ON SCHEMA public FROM PUBLIC;
    ELSE
        RAISE NOTICE 'public schema is not owned by %; leaving its CREATE privilege as it is', current_user;
    END IF;
END
$$;

COMMIT;
