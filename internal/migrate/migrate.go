// Package migrate applies the schema files embedded under sql/ in order and
// records each one in schema_migrations. It runs as `simple-host migrate`
// from the pod's init container, under an advisory lock so a second pod
// starting at the same moment waits instead of racing, and it is what lets
// the server refuse to start against a schema it cannot safely run on.
package migrate

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
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

// Defaults for Apply's session. A migration waiting on a lock held by live
// traffic would otherwise queue every later query behind it; failing instead
// lets the init container retry. A file may still SET LOCAL tighter values.
const (
	DefaultLockWait         = 5 * time.Minute
	sessionLockTimeout      = "15s"
	sessionStatementTimeout = "10min"
)

// Apply runs every pending migration in order and returns the names applied.
// Each file and its schema_migrations row commit in one transaction, so a
// failure leaves neither behind and the run stops there. lockWait bounds how
// long it waits for another migrator; zero means DefaultLockWait.
func Apply(ctx context.Context, db *sql.DB, lockWait time.Duration, report func(string)) ([]string, error) {
	if report == nil {
		report = func(string) {}
	}
	if lockWait <= 0 {
		lockWait = DefaultLockWait
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := takeLock(ctx, conn, lockWait, report); err != nil {
		return nil, err
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = conn.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, lockKey)
		_, _ = conn.ExecContext(cleanup, `RESET lock_timeout; RESET statement_timeout`)
	}()
	if _, err := conn.ExecContext(ctx, `SET lock_timeout = '`+sessionLockTimeout+`'; SET statement_timeout = '`+sessionStatementTimeout+`'`); err != nil {
		return nil, fmt.Errorf("set migration timeouts: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS backward_compatible boolean NOT NULL DEFAULT false`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	pending, err := Pending(ctx, db)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, m := range pending {
		report("applying " + m.Name)
		if err := applyOne(ctx, conn, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

// takeLock polls pg_try_advisory_lock until it succeeds or wait runs out, so
// a migrator stuck behind a dead peer fails with a reason instead of hanging.
func takeLock(ctx context.Context, conn *sql.Conn, wait time.Duration, report func(string)) error {
	deadline := time.Now().Add(wait)
	for {
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil {
			return fmt.Errorf("take migration lock: %w", err)
		}
		if got {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("take migration lock: another migrator held it for more than %s", wait)
		}
		report("waiting for another migrator to finish")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func applyOne(ctx context.Context, conn *sql.Conn, m Migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, withoutTransactionControl(m.body)); err != nil {
		return fmt.Errorf("apply %s: %w", m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, backward_compatible) VALUES ($1, $2, $3)`, m.Version, m.Name, m.BackwardCompatible()); err != nil {
		return fmt.Errorf("record %s: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", m.Name, err)
	}
	return nil
}

// withoutTransactionControl drops a file's own top-level `BEGIN;` and
// `COMMIT;` lines. Many files carry them so that they are atomic when run by
// hand with psql; under Apply the file already runs inside a transaction that
// also records it, and a COMMIT of its own would end that transaction before
// the record is written. PL/pgSQL's BEGIN has no semicolon and is untouched.
func withoutTransactionControl(body string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "BEGIN;", "COMMIT;":
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// CompatibleMarker, as a file's first line, declares the migration additive:
// the previous release's binary still runs correctly against it (new tables,
// new nullable or defaulted columns, new indexes). Only such migrations let an
// image be rolled back without restoring the database.
const CompatibleMarker = "-- simple-host: backward-compatible"

// BackwardCompatible reports whether the file declares CompatibleMarker.
func (m Migration) BackwardCompatible() bool {
	first, _, _ := strings.Cut(m.body, "\n")
	return strings.TrimSpace(first) == CompatibleMarker
}

// Check is the server's startup gate. Every version this binary embeds must
// be applied: older means `migrate` has not run. A schema newer than the
// binary is accepted only when every newer migration was recorded as
// backward-compatible, which is what makes rolling an image back possible;
// anything else is a rolled-back image looking at a schema that has moved on,
// and failing here beats failing on a query.
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
	newer := false
	for version := range applied {
		if version > latest {
			newer = true
		}
	}
	if !newer {
		return nil
	}
	// to_jsonb reads the column when it exists and yields NULL when a
	// database was migrated before it did, so this never errors on either.
	var blocking sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT min(version) FROM schema_migrations m
		WHERE version > $1
		  AND NOT COALESCE((to_jsonb(m) ->> 'backward_compatible')::boolean, false)`, latest).Scan(&blocking); err != nil {
		return fmt.Errorf("read schema compatibility: %w", err)
	}
	if blocking.Valid {
		return fmt.Errorf("schema version %d is newer than this binary knows (%d) and is not backward-compatible; deploy the newer image, or restore the database to before the upgrade", blocking.Int64, latest)
	}
	return nil
}

// AppRoleName is the least-privilege role the server connects as once
// migration 0020 has granted it SELECT/INSERT/UPDATE/DELETE on the
// application tables. It has no default password: migration
// files carry no secrets, so SetAppRolePassword sets it separately, from an
// operator-supplied value, every time migrate runs.
const AppRoleName = "simplehost_app"

// SetAppRolePassword sets AppRoleName's login password. Called after Apply
// succeeds, as the owning role, with the password the server's own
// connection will use (DB_APP_PASSWORD). Only a SCRAM-SHA-256 verifier,
// computed here, reaches the server: Postgres stores a pre-computed verifier
// as-is, so the plaintext never appears in statement text, bind parameters,
// server logs, pgaudit or pg_stat_statements. The verifier's alphabet
// (base64, '$', ':') needs no quoting beyond the literal's own quotes.
func SetAppRolePassword(ctx context.Context, db *sql.DB, password string) error {
	verifier, err := scramSHA256Verifier(password)
	if err != nil {
		return err
	}
	stmt := "ALTER ROLE " + AppRoleName + " WITH LOGIN PASSWORD '" + verifier + "'"
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("set %s password: %w", AppRoleName, err)
	}
	return nil
}

// scramIterations matches Postgres's default scram_iterations.
const scramIterations = 4096

// scramSHA256Verifier builds the verifier Postgres itself would store
// (RFC 5802/7677): SCRAM-SHA-256$<iter>:<salt>$<StoredKey>:<ServerKey>.
// Postgres runs a password through SASLprep, which is the identity for
// printable ASCII; anything else is refused rather than risk a verifier the
// server would never match.
func scramSHA256Verifier(password string) (string, error) {
	if password == "" {
		return "", errors.New("app role password must not be empty")
	}
	for i := 0; i < len(password); i++ {
		if password[i] < 0x20 || password[i] > 0x7e {
			return "", errors.New("app role password must be printable ASCII")
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	storedKey := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations, b64(salt), b64(storedKey[:]), b64(mac("Server Key"))), nil
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
// is meant to connect as AppRoleName, the least-privilege role; this catches
// every way a deployment could end up connecting as the owning role instead
// — a DB_DSN naming it, a password-file override that won, a hand-edited
// manifest.
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
