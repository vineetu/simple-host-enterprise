-- simple-host: backward-compatible
-- Marked compatible because it only adds a table, a trigger on users and a
-- lock inside the 0048 trigger's function, none of which older code writes
-- or depends on. Rolled back to a binary without it:
--   * a renamed person keeps their new name, and their old site addresses
--     keep redirecting (site_redirects, 0043);
--   * the old labels stay held against sign-in and renames by this trigger;
--   * erasing a renamed person does not copy their held labels into
--     erased_owner_labels, but the holds survive the users row (user_id is
--     set to NULL, which the trigger treats as held against everyone), so
--     the labels stay held;
--   * older code's legacy-team mapping (a pre-v1.3 team address answering
--     for "team-<label>") does not know about held labels: once someone
--     creates a team named after a held label, the old owner page of the
--     renamed person answers as that team's page. Site addresses are not
--     affected (they are redirects recorded per site).
--
-- An admin renames a person's address after a name change (POST
-- /api/admin/users/{username}/rename). renamed_owner_labels holds each label
-- a person had before, so nobody else signs in as it and inherits the links
-- that now redirect to them: the trigger below refuses a username whose
-- label another person held, with the same unique violation, on the same
-- index name, that a live account's label gets (the one 0048 raises for an
-- erased person's label), so sign-in moves on to a suffixed name. The person
-- who held it may take it back; the row is then deleted. Erasing the person
-- moves their held labels to erased_owner_labels.
--
-- Every check on a label takes a transaction-scoped advisory lock on it
-- first (owner_label_lock below; RenamePerson and ErasePerson take the same
-- lock before they change which labels are held). Without it a sign-in
-- running while a rename is uncommitted passes the hold check (it cannot see
-- the uncommitted row), waits on the unique index for the old username, and
-- takes the label once the rename commits. With it the sign-in waits, then
-- re-reads the holds with a fresh snapshot (READ COMMITTED) and is refused.
-- The 0048 function is redefined here with the same lock, since it fires
-- first (triggers run in name order).
--
-- The label expression must stay what ownerLabel computes and what
-- users_owner_label_idx (migration 0018) indexes.
--
-- Lock duration: one new empty table and a trigger on users (a brief
-- ACCESS EXCLUSIVE lock to add it, bounded by lock_timeout).

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS renamed_owner_labels (
    owner_label text PRIMARY KEY,
    -- NULL once the person is erased by a binary older than this
    -- migration: still held, against everyone.
    user_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    renamed_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS renamed_owner_labels_user_idx ON renamed_owner_labels (user_id);

-- The lock key is the one internal/db/rename.go (lockOwnerLabels) takes.
CREATE OR REPLACE FUNCTION owner_label_lock(label text) RETURNS void
LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(hashtext('owner_label:' || label)) $$;

CREATE OR REPLACE FUNCTION users_refuse_erased_label() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM public.owner_label_lock(lower(replace(NEW.username, '.', '-')));
    IF EXISTS (SELECT 1 FROM public.erased_owner_labels
               WHERE owner_label = lower(replace(NEW.username, '.', '-'))) THEN
        RAISE unique_violation USING
            MESSAGE = 'username label is held after an erasure',
            CONSTRAINT = 'users_owner_label_idx';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION users_refuse_renamed_label() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM public.owner_label_lock(lower(replace(NEW.username, '.', '-')));
    IF EXISTS (SELECT 1 FROM public.renamed_owner_labels
               WHERE owner_label = lower(replace(NEW.username, '.', '-'))
                 AND user_id IS DISTINCT FROM NEW.id) THEN
        RAISE unique_violation USING
            MESSAGE = 'username label is held after a rename',
            CONSTRAINT = 'users_owner_label_idx';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS users_refuse_renamed_label ON users;
CREATE TRIGGER users_refuse_renamed_label
    BEFORE INSERT OR UPDATE OF username ON users
    FOR EACH ROW EXECUTE FUNCTION users_refuse_renamed_label();

GRANT SELECT, INSERT, DELETE ON TABLE renamed_owner_labels TO simplehost_app;

COMMIT;
