-- Drop the foreign keys on audit_events and access_log (Phase 7
-- verification finding: DELETE /api/sites/{site} always 500s).
--
-- deleteSiteForTarget (internal/handler/site.go) runs db.DeleteSite and then
-- h.audit.RecordTx for the site_delete event in the same transaction. By the
-- time the audit INSERT runs, the site row it points at (site_id) is
-- already gone, so audit_events_site_id_fkey rejects it immediately --
-- foreign keys are checked per-statement (NOT DEFERRABLE here), so ordering
-- the two statements the other way round would not help either. The same
-- shape exists for team_delete (internal/handler/team.go): that path
-- currently avoids the failure only because it happens to record the audit
-- row *before* calling db.DeleteTeam, so the FK is still satisfied at
-- INSERT time and ON DELETE SET NULL quietly nulls team_id out from under
-- it afterwards. That ordering is incidental, not a rule anyone enforces,
-- and the same problem will resurface the moment a user-removal path is
-- added (design.md's audit trail is explicitly meant to outlive the users,
-- keys, owners, sites and teams it describes).
--
-- The real fix is schema-level: audit_events and access_log are an
-- append-only record of what happened, not a live view of what still
-- exists, so they must not be able to block deleting their own subjects,
-- and must not silently lose the identifying id when a subject is deleted
-- either (ON DELETE SET NULL, migration 0027's original choice, erases the
-- very thing an auditor would want to look up later). Dropping the
-- constraints keeps every column (a site_id/owner_id/team_id/actor_id/
-- key_id/user_id/session_id on an old row still names the subject that
-- used to exist there) and every index that serves lookups by those
-- columns; only the referential-integrity enforcement goes away. Postgres
-- does not orphan-check on drop, so this is safe to run against rows that
-- already reference deleted subjects.
--
-- Idempotent: DROP CONSTRAINT IF EXISTS so re-running this migration (or
-- running it against a database where these were already hand-dropped
-- during incident triage) does nothing on a second pass.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_actor_id_fkey;
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_key_id_fkey;
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_owner_id_fkey;
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_site_id_fkey;
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_team_id_fkey;

ALTER TABLE access_log DROP CONSTRAINT IF EXISTS access_log_user_id_fkey;
ALTER TABLE access_log DROP CONSTRAINT IF EXISTS access_log_session_id_fkey;

COMMIT;
