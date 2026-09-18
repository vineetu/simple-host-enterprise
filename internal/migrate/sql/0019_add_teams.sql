-- Teams: a namespace that owns sites without being a person.
--
-- A team is a row in users with kind = 'team' and no api_key. People are
-- linked to it by team_members and act with their own personal keys, so
-- nothing here creates a credential and nothing revokes one.
--
-- There is deliberately no role column. One role: you are in the team or you
-- are not. See docs/teams/design.md, "One role. You are in the team, or you
-- are not." A role column added later is one additive migration; a column
-- nothing reads is a second source of truth free to drift.
--
-- The number is assigned by the table in docs/namespaces/roadmap.md phase 1,
-- which reserved 0018 for the canonical-name index and 0019 for this.
--
-- Applied by hand through a port-forward, before the binary that reads any of
-- it is deployed:
--
--     psql -v ON_ERROR_STOP=1 -f scripts/migrations/0019_add_teams.sql
--
-- Re-runnable. Every statement is guarded, so a run that fails on lock_timeout
-- leaves nothing behind and can simply be run again. The running (old) binary
-- never reads any of these objects, so applying this early is safe; what is
-- not safe is deploying a binary that needs them before this has been applied,
-- which is why the readiness probe checks for them.

BEGIN;

-- Fail fast rather than queue behind a long transaction while holding DDL
-- locks on users, which every authenticated request reads. On timeout the
-- whole file rolls back; rerun it.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- users.email and users.email_source already exist: migration 0017 shipped
-- them ahead of this work and backfilled them. Do not re-add them here.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'person';

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kind_check;
ALTER TABLE users ADD CONSTRAINT users_kind_check CHECK (kind IN ('person', 'team'));

CREATE TABLE IF NOT EXISTS team_members (
    team_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT team_members_pkey PRIMARY KEY (team_id, user_id),
    CONSTRAINT team_members_not_self CHECK (team_id <> user_id)
);

-- Membership is read in both directions: "who is in this team" from the
-- primary key, and "which teams is this person in" from this index, which is
-- the one every authenticated site request uses.
CREATE INDEX IF NOT EXISTS team_members_user_idx ON team_members (user_id, team_id);

CREATE TABLE IF NOT EXISTS team_audit (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_id   uuid REFERENCES users(id) ON DELETE SET NULL,
    action     text NOT NULL,
    subject_id uuid REFERENCES users(id) ON DELETE SET NULL,
    detail     text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS team_audit_team_idx ON team_audit (team_id, created_at DESC);

-- Database-level half of the takeover guard.
--
-- A team namespace must never hold an api_key. The handler refuses kind =
-- 'team' on every /api/auth branch and every key-writing UPDATE carries a
-- kind = 'person' predicate, but a rolled-back binary has neither, and a
-- hand-written UPDATE has neither. This is the guard that survives both.
CREATE OR REPLACE FUNCTION teams_have_no_key() RETURNS trigger AS $$
BEGIN
    IF NEW.kind = 'team' AND NEW.api_key IS NOT NULL THEN
        RAISE EXCEPTION 'team namespace % cannot hold an api_key', NEW.username;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_teams_have_no_key ON users;
CREATE TRIGGER users_teams_have_no_key
    BEFORE INSERT OR UPDATE OF api_key, kind ON users
    FOR EACH ROW EXECUTE FUNCTION teams_have_no_key();

-- Membership rows must link a team to a person at the moment they are
-- written. Enforced here rather than trusted to application SQL, because a
-- name that stops being a person between lookup and insert would otherwise
-- leave a row the resolvers would honour.
CREATE OR REPLACE FUNCTION team_members_kinds() RETURNS trigger AS $$
BEGIN
    IF (SELECT kind FROM users WHERE id = NEW.team_id) <> 'team'
       OR (SELECT kind FROM users WHERE id = NEW.user_id) <> 'person' THEN
        RAISE EXCEPTION 'team_members must link a team to a person';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS team_members_kinds ON team_members;
CREATE TRIGGER team_members_kinds
    BEFORE INSERT OR UPDATE ON team_members
    FOR EACH ROW EXECUTE FUNCTION team_members_kinds();

-- The other half of that check. The trigger above fires only when a
-- membership row is written, so it does not see a later
-- UPDATE users SET kind = ... The dangerous direction is a team becoming a
-- person: every team_members row keeps pointing at it, and the membership
-- join then hands every former member full access to that person's own sites.
-- No application path changes kind, so this is reachable only by hand-written
-- SQL, which is exactly what these triggers exist to backstop.
CREATE OR REPLACE FUNCTION users_kind_locked_by_membership() RETURNS trigger AS $$
BEGIN
    IF NEW.kind <> OLD.kind
       AND EXISTS (SELECT 1 FROM team_members WHERE team_id = OLD.id OR user_id = OLD.id) THEN
        RAISE EXCEPTION 'cannot change kind of % while team_members rows reference it', OLD.username;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_kind_locked ON users;
CREATE TRIGGER users_kind_locked
    BEFORE UPDATE OF kind ON users
    FOR EACH ROW EXECUTE FUNCTION users_kind_locked_by_membership();

COMMIT;
