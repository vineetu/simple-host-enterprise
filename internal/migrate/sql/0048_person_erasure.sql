-- simple-host: backward-compatible
-- Marked compatible because it only adds tables, a trigger and a function
-- no older code calls. Rolled back, an erased person's name stays held (the
-- trigger still refuses it) and erasure is simply not offered.
--
-- Erasing a person (an admin's "Delete person and all data") removes their
-- users row. Two things outlive it:
--
--   * erased_owner_labels holds the address label they had, so nobody who
--     signs in later is handed "<label>.<base>" and inherits links that used
--     to point at the erased person's work. The trigger below refuses a
--     username whose label is held with the same unique violation, on the
--     same index name, that a live account's label gets, so sign-in moves on
--     to a suffixed name exactly as it does for a taken one.
--   * access_log_erase_visitor deletes the access-log rows where the erased
--     person was the visitor. The application role keeps INSERT/SELECT only
--     on access_log; this SECURITY DEFINER function is the one delete it is
--     given, and it takes nothing but the id of an account that no longer
--     exists (it is called after the users row is deleted, in the same
--     transaction). audit_events rows are not touched: they are hash-chained
--     and keep the opaque id until retention prunes their partition.
--   * erased_identities holds a SHA-256 of the sign-in identity (issuer and
--     subject) and of the provider-vouched email, so an erased person who
--     is still in the identity provider cannot sign straight back in to a
--     fresh account. An admin can allow sign-in again (the row is deleted).
--
-- SECURITY DEFINER functions here and in 0049 pin search_path to
-- pg_catalog, public, pg_temp and name every table with its schema: with
-- pg_temp left out of the list Postgres searches it first, so a caller could
-- shadow a table with a temporary one and run its own trigger as the owner.
BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS erased_owner_labels (
    owner_label text PRIMARY KEY,
    erased_at   timestamptz NOT NULL DEFAULT now()
);

-- The label expression must stay what ownerLabel computes and what
-- users_owner_label_idx (migration 0018) indexes.
CREATE OR REPLACE FUNCTION users_refuse_erased_label() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.erased_owner_labels
               WHERE owner_label = lower(replace(NEW.username, '.', '-'))) THEN
        RAISE unique_violation USING
            MESSAGE = 'username label is held after an erasure',
            CONSTRAINT = 'users_owner_label_idx';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS users_refuse_erased_label ON users;
CREATE TRIGGER users_refuse_erased_label
    BEFORE INSERT OR UPDATE OF username ON users
    FOR EACH ROW EXECUTE FUNCTION users_refuse_erased_label();

CREATE TABLE IF NOT EXISTS erased_identities (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_hash text,
    email_hash   text,
    erased_at    timestamptz NOT NULL DEFAULT now(),
    erased_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    CHECK (subject_hash IS NOT NULL OR email_hash IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS erased_identities_subject_idx ON erased_identities (subject_hash);
CREATE INDEX IF NOT EXISTS erased_identities_email_idx ON erased_identities (email_hash);

CREATE OR REPLACE FUNCTION access_log_erase_visitor(p_user_id uuid)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    removed bigint;
BEGIN
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'access_log_erase_visitor: a user id is required';
    END IF;
    -- Only an erased account's visits: a live person's trail is not the
    -- application's to delete.
    IF EXISTS (SELECT 1 FROM public.users WHERE id = p_user_id) THEN
        RAISE EXCEPTION 'access_log_erase_visitor: % is still an account; delete it first', p_user_id;
    END IF;
    DELETE FROM public.access_log WHERE user_id = p_user_id;
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END $$;

REVOKE ALL ON FUNCTION access_log_erase_visitor(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION access_log_erase_visitor(uuid) TO simplehost_app;
GRANT SELECT, INSERT ON TABLE erased_owner_labels TO simplehost_app;
GRANT SELECT, INSERT, DELETE ON TABLE erased_identities TO simplehost_app;

COMMIT;
