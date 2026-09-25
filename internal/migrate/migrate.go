// Package migrate applies the schema files embedded under sql/ in order and
// records each one in schema_migrations. It runs as `simple-host migrate`
// from the pod's init container, under an advisory lock so a second pod
// starting at the same moment waits instead of racing, and it is what lets
// the server refuse to start against a schema it does not know.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed sql/*.sql
var files embed.FS

// lockKey is the advisory lock every migrator takes. Arbitrary, fixed, and
// unlikely to collide with anything else in the database.
const lockKey = 7275817

// Migration is one embedded schema file.
type Migration struct {
	Version int
	Name    string
	body    string
}

// All returns the embedded migrations in version order.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		if prior, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", prior, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(files, "sql/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: version, Name: name, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// versionOf reads the leading digits of a file name: 0017_add_user_email.sql
// is version 17. Anything else is a packaging mistake, not a migration.
func versionOf(name string) (int, error) {
	digits := name
	if i := strings.IndexByte(name, '_'); i > 0 {
		digits = name[:i]
	}
	digits = strings.TrimSuffix(digits, ".sql")
	version, err := strconv.Atoi(digits)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("migration %q does not start with a positive version number", name)
	}
	return version, nil
}

// Latest is the newest version this binary embeds.
func Latest() (int, error) {
	all, err := All()
	if err != nil {
		return 0, err
	}
	if len(all) == 0 {
		return 0, errors.New("no migrations embedded")
	}
	return all[len(all)-1].Version, nil
}

// Applied returns the versions recorded in schema_migrations. ErrNoTable is
// returned when the table does not exist, which is a database nothing has
// ever migrated.
func Applied(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, ErrNoTable
		}
		return nil, err
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// ErrNoTable means schema_migrations does not exist.
var ErrNoTable = errors.New("schema_migrations table does not exist; run `simple-host migrate`")

// Pending returns the embedded migrations not yet recorded, in order.
func Pending(ctx context.Context, db *sql.DB) ([]Migration, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	applied, err := Applied(ctx, db)
	if err != nil && !errors.Is(err, ErrNoTable) {
		return nil, err
	}
	var pending []Migration
	for _, m := range all {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// Apply runs every pending migration in order and returns the names applied.
// Each file runs as one script exactly as written: several files carry their
// own BEGIN/COMMIT, so wrapping them in another transaction would either
// nest or commit early. The record is inserted after the script succeeds, so
// a failure leaves the file unrecorded and the run stops there.
func Apply(ctx context.Context, db *sql.DB, report func(string)) ([]string, error) {
	if report == nil {
		report = func(string) {}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) }()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	pending, err := Pending(ctx, db)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, m := range pending {
		report("applying " + m.Name)
		if _, err := conn.ExecContext(ctx, m.body); err != nil {
			return applied, fmt.Errorf("apply %s: %w", m.Name, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
			return applied, fmt.Errorf("record %s: %w", m.Name, err)
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

// Check is the server's startup gate. The database must hold exactly the
// versions this binary embeds: older means `migrate` has not run, and newer
// means a rolled-back image is looking at a schema that has moved on, which
// is the failure expand/contract exists to make loud rather than sporadic.
func Check(ctx context.Context, db *sql.DB) error {
	all, err := All()
	if err != nil {
		return err
	}
	applied, err := Applied(ctx, db)
	if err != nil {
		return err
	}
	latest := all[len(all)-1].Version
	var missing []string
	for _, m := range all {
		if !applied[m.Version] {
			missing = append(missing, m.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("schema is behind this binary: %d migration(s) pending, first %s; run `simple-host migrate`", len(missing), missing[0])
	}
	for version := range applied {
		if version > latest {
			return fmt.Errorf("schema version %d is newer than this binary knows (%d); deploy the newer image or restore the schema before rolling back", version, latest)
		}
	}
	return nil
}

// AppRoleName is the least-privilege role the server connects as once
// migration 0020 has granted it SELECT/INSERT/UPDATE/DELETE on the
// application tables (design 9.3). It has no default password: migration
// files carry no secrets, so SetAppRolePassword sets it separately, from an
// operator-supplied value, every time migrate runs.
const AppRoleName = "simplehost_app"

// SetAppRolePassword sets AppRoleName's login password. Called after Apply
// succeeds, as the owning role, with the password the server's own
// connection will use (DB_APP_PASSWORD). The password never appears in SQL
// text this function builds: it is bound as an ordinary argument to
// Postgres's own format(), whose %L verb quotes and escapes it, and only the
// resulting pre-quoted statement is executed. This avoids the SQL injection
// a naive Sprintf into "ALTER ROLE ... PASSWORD '<password>'" would risk,
// without requiring extended-protocol parameter binding that ALTER ROLE's
// grammar does not accept in that position.
func SetAppRolePassword(ctx context.Context, db *sql.DB, password string) error {
	if password == "" {
		return errors.New("app role password must not be empty")
	}
	var stmt string
	err := db.QueryRowContext(
		ctx,
		`SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', $1::text, $2::text)`,
		AppRoleName, password,
	).Scan(&stmt)
	if err != nil {
		return fmt.Errorf("build ALTER ROLE statement: %w", err)
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("set %s password: %w", AppRoleName, err)
	}
	return nil
}

// isUndefinedTable matches PostgreSQL's 42P01 without depending on a driver
// type: lib/pq and pgx both put the code in the message.
func isUndefinedTable(err error) bool {
	type coder interface{ SQLState() string }
	var c coder
	if errors.As(err, &c) {
		return c.SQLState() == "42P01"
	}
	return strings.Contains(err.Error(), "42P01") || strings.Contains(err.Error(), "does not exist")
}

// CheckLeastPrivilege refuses a server connection whose role could rewrite
// the audit trail: one that owns audit_events (or can act as its owner, a
// superuser included) or holds UPDATE, DELETE or TRUNCATE on it. The server
// is meant to connect as AppRoleName (design 9.3); this catches every way a
// deployment could end up connecting as the owning role instead — a DB_DSN
// naming it, a password-file override that won, a hand-edited manifest.
func CheckLeastPrivilege(ctx context.Context, db *sql.DB) error {
	const query = `
		SELECT current_user,
		       has_table_privilege('audit_events', 'UPDATE')
		    OR has_table_privilege('audit_events', 'DELETE')
		    OR has_table_privilege('audit_events', 'TRUNCATE')
		    OR pg_has_role((SELECT relowner FROM pg_class WHERE oid = 'audit_events'::regclass), 'MEMBER')
	`
	var role string
	var tooStrong bool
	if err := db.QueryRowContext(ctx, query).Scan(&role, &tooStrong); err != nil {
		return fmt.Errorf("check database role privileges: %w", err)
	}
	if tooStrong {
		return fmt.Errorf("the server is connected as database role %q, which can alter or delete audit_events; connect as %s (set DB_APP_USER and DB_APP_PASSWORD) — the owning role is for migrate and prune only", role, AppRoleName)
	}
	return nil
}
