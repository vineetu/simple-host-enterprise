-- Least-privilege application role.
--
-- Every migration, including this one, runs as the owning role: the
-- Postgres superuser the in-cluster component provisions with POSTGRES_USER,
-- or a managed database's owner grant. The server itself never connects as
-- that role once this migration has applied; it connects as
-- simplehost_app, which can read and write application data but cannot
-- alter the schema, cannot TRUNCATE, and has no access outside this
-- database. `simple-host migrate` sets this role's login password from
-- DB_APP_PASSWORD immediately after applying pending migrations (never from
-- a value embedded in a migration file); see cmd/server/subcommands.go.
--
-- Numbered 0020: internal/migrate.Apply requires the embedded chain to be
-- gap-free (TestEmbeddedMigrationsAreOrderedAndUnique), so migrations are
-- numbered by merge order, not by when they were proposed. Identity's
-- migrations were pencilled in at 0020-0022, but since this file lands
-- first and takes 0020, later migrations number from whatever is next
-- when they actually merge, not from the earlier illustrative numbers.
--
-- `audit_events` (migration 0027) is deliberately not granted here: the
-- app role gets INSERT and a narrow, function-mediated UPDATE on it, never
-- DELETE, which that migration wires up. Any migration that introduces a
-- new application table must grant this role's
-- privileges on that table in the same file; nothing here does that
-- automatically, on purpose, so a new table defaults to no access rather
-- than to whatever this file happened to grant.

-- Re-runnable, like every migration here must be (internal/migrate.Apply
-- records a file's success only after it completes, so a crash between the
-- two re-runs the file next time): CREATE ROLE has no IF NOT EXISTS, so the
-- existence check is done by hand.
--
-- The check-then-create is also not safe against a second, concurrent
-- migrator: unlike every other object this migration chain creates, a
-- role is cluster-global, not scoped to one database, so two migrations
-- running against two different databases on the same Postgres server at
-- the same moment can both pass the IF NOT EXISTS check before either
-- commits its CREATE ROLE, and the second then fails with
-- duplicate_object. In normal operation this migration runs once, under
-- an advisory lock (internal/migrate.Apply), against one server that owns
-- one migration history — but a live-Postgres test suite that spins up
-- its own fresh throwaway database per test (internal/db's assetsTestDB,
-- internal/audit's openPruneTestDB) is exactly this second case: several
-- packages' test binaries each migrate their own fresh database
-- concurrently against the same physical server, all racing to create
-- this one shared role. Caught here, not with an advisory lock around the
-- check, because a lock only protects callers that agree to take it and
-- every migrator already takes internal/migrate.Apply's own lock for its
-- own connection — it is the race between separate lock-holding sessions,
-- each correct on its own, that this exists to make harmless.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'simplehost_app') THEN
        CREATE ROLE simplehost_app NOLOGIN;
    END IF;
EXCEPTION
    -- Another concurrent migrator won the race between the existence
    -- check above and this CREATE ROLE. The role exists either way, which
    -- is all this block ever promised. Both conditions are caught, not
    -- just duplicate_object: CREATE ROLE's own internal name lookup is
    -- what normally raises the friendlier "role already exists"
    -- (duplicate_object, 42710) once it sees a committed row, but two
    -- CREATE ROLE statements racing closely enough both pass that lookup
    -- before either has inserted, and the loser then hits a raw
    -- unique_violation (23505) on pg_authid's own name index instead —
    -- confirmed by reproducing it directly (fifteen concurrent sessions
    -- against a role-less database; three failed with unique_violation on
    -- pg_authid_rolname_index until this line was added).
    WHEN duplicate_object OR unique_violation THEN
        NULL;
END
$$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO simplehost_app', current_database());
END
$$;

GRANT USAGE ON SCHEMA public TO simplehost_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE
    users,
    sites,
    versions,
    site_daily_analytics,
    ai_usage,
    reset_requests,
    site_file_downloads,
    site_collaborators,
    site_search_documents,
    site_search_queue,
    site_search_index_status,
    site_search_queries,
    site_search_impressions,
    site_search_clicks,
    key_reissues,
    team_members,
    team_audit
TO simplehost_app;

-- schema_migrations is migration bookkeeping, not application data. The app
-- role gets SELECT only, because the server's own startup gate
-- (internal/migrate.Check) reads it before serving a single request; it gets
-- no INSERT/UPDATE/DELETE, so a compromised server process can still read
-- the recorded version but cannot forge or roll it back.
GRANT SELECT ON TABLE schema_migrations TO simplehost_app;
