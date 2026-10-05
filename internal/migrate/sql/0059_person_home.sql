-- simple-host: backward-compatible
-- Personal presentation settings; older releases ignore these additions.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE users ADD COLUMN home_site uuid REFERENCES sites(id) ON DELETE SET NULL,
                  ADD COLUMN showcase_bio text NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN showcase_pinned boolean NOT NULL DEFAULT false,
                  ADD COLUMN showcase_order integer NOT NULL DEFAULT 0 CHECK (showcase_order BETWEEN 0 AND 1000000);
CREATE FUNCTION clear_person_home() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.deleted_at IS NOT NULL OR NEW.user_id IS DISTINCT FROM OLD.user_id THEN
    UPDATE users SET home_site = NULL WHERE home_site = NEW.id;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER clear_person_home AFTER UPDATE OF deleted_at, user_id ON sites
FOR EACH ROW EXECUTE FUNCTION clear_person_home();
COMMIT;
