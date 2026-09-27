-- simple-host: backward-compatible
-- Marked compatible because it only adds a table and a trigger on it that
-- older code never writes: rolled back, a renamed person keeps their new
-- name, their old addresses keep redirecting (site_redirects, 0043), and the
-- old label stays held by this trigger.
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
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    renamed_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS renamed_owner_labels_user_idx ON renamed_owner_labels (user_id);

CREATE OR REPLACE FUNCTION users_refuse_renamed_label() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
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
