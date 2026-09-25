package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"
)

// A renamed account keeps the address it signed up with while its username
// changes, so the address is the only thing that still identifies its owner
// on a first OIDC sign-in (before oidc_sub is bound). Without the address
// lookup, a renamed owner signing in for the first time under OIDC would be
// bound to a brand new, empty account instead of their own.
func TestOIDCResolveUserFindsARenamedAccountByItsAddress(t *testing.T) {
	// The account answers to preet.kaur; the address still derives
	// amanpreet.kaur, which matches no account by name.
	conn := &emailLookupConn{
		byEmail: map[string]fakeUser{
			"amanpreet.kaur@example.com": {id: "u-1", username: "preet.kaur"},
		},
	}
	database := sql.OpenDB(&emailLookupConnector{conn: conn})
	t.Cleanup(func() { _ = database.Close() })
	h := newTestAuthHandler(database, "example.com")

	user, _, err := h.resolveUser(context.Background(), "sub-1", "amanpreet.kaur@example.com", "", false)
	if err != nil {
		t.Fatalf("resolveUser: %v", err)
	}
	if user.ID != "u-1" || user.Username != "preet.kaur" {
		t.Fatalf("resolveUser resolved to %+v, want the existing account u-1/preet.kaur", user)
	}
	if conn.boundUserID != "u-1" {
		t.Fatalf("BindOIDCSub target = %q, want u-1: binding by the derived name would target an account that does not exist", conn.boundUserID)
	}
	if conn.insertCalled {
		t.Fatal("a matching existing account must be bound, not duplicated by an INSERT")
	}
}

// No account holds the address at all: a brand new person is created, the
// ordinary case for a first sign-in.
func TestOIDCResolveUserCreatesWhenNoAddressMatches(t *testing.T) {
	conn := &emailLookupConn{byEmail: map[string]fakeUser{}}
	database := sql.OpenDB(&emailLookupConnector{conn: conn})
	t.Cleanup(func() { _ = database.Close() })
	h := newTestAuthHandler(database, "example.com")

	if _, _, err := h.resolveUser(context.Background(), "sub-2", "nobody.here@example.com", "", false); err != nil {
		t.Fatalf("resolveUser: %v", err)
	}
	if !conn.insertCalled {
		t.Fatal("an unmatched address must create a new account")
	}
	if conn.insertedUsername != "nobody.here" {
		t.Fatalf("created username = %q, want the derived name nobody.here", conn.insertedUsername)
	}
}

// Two accounts on one address is a state the schema permits on purpose. The
// lookup must refuse to pick one rather than hand somebody another person's
// account, and fall back to creating a new one under the derived name.
func TestOIDCResolveUserRefusesToGuessWhenAnAddressIsShared(t *testing.T) {
	conn := &emailLookupConn{
		byEmail: map[string]fakeUser{
			"shared.name@example.com": {id: "u-1", username: "shared.name"},
		},
		duplicateEmail: "shared.name@example.com",
	}
	database := sql.OpenDB(&emailLookupConnector{conn: conn})
	t.Cleanup(func() { _ = database.Close() })
	h := newTestAuthHandler(database, "example.com")

	if _, _, err := h.resolveUser(context.Background(), "sub-3", "shared.name@example.com", "", false); err != nil {
		t.Fatalf("resolveUser: %v", err)
	}
	if conn.boundUserID != "" {
		t.Fatal("an ambiguous address must not be bound to either account")
	}
	if !conn.insertCalled {
		t.Fatal("an ambiguous address must fall back to creating a new account")
	}
}

// newTestAuthHandler builds an AuthHandler with no OIDC provider (these
// tests exercise resolveUser/createUser directly, never the provider) and
// the given allowed email domain, which is what gates binding an existing
// account by email at all.
func newTestAuthHandler(database *sql.DB, allowedDomain string) *AuthHandler {
	return NewAuthHandler(
		database, nil,
		OIDCClaimConfig{AllowedEmailDomains: []string{allowedDomain}},
		nil, time.Hour, time.Hour, nil, HostModel{}, "https://example.com",
	)
}

type fakeUser struct {
	id, username string
}

type emailLookupConnector struct{ conn *emailLookupConn }

func (c *emailLookupConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *emailLookupConnector) Driver() driver.Driver                        { return noRowsDriver{} }

type emailLookupConn struct {
	byEmail        map[string]fakeUser
	duplicateEmail string

	boundUserID      string
	insertCalled     bool
	insertedUsername string
	unmatched        []string
}

func (c *emailLookupConn) Prepare(string) (driver.Stmt, error) {
	return nil, io.ErrUnexpectedEOF
}

func (c *emailLookupConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "SET oidc_sub"):
		if len(args) > 0 {
			if id, ok := args[0].Value.(string); ok {
				c.boundUserID = id
			}
		}
		return driver.RowsAffected(1), nil
	case strings.Contains(query, "SET is_admin"):
		return driver.RowsAffected(1), nil
	}
	return driver.RowsAffected(0), nil
}
func (c *emailLookupConn) Close() error              { return nil }
func (c *emailLookupConn) Begin() (driver.Tx, error) { return noRowsTx{}, nil }
func (c *emailLookupConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return noRowsTx{}, nil
}

func (c *emailLookupConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "WHERE oidc_sub = $1"):
		// Every test here starts from an unbound sub.
		return &userRows{columns: selectedColumns(query)}, nil
	case strings.Contains(query, "WHERE email = $1"):
		email, _ := args[0].Value.(string)
		user, found := c.byEmail[email]
		if !found {
			return &userRows{columns: selectedColumns(query)}, nil
		}
		rows := &userRows{users: []fakeUser{user}, columns: selectedColumns(query)}
		if email == c.duplicateEmail {
			rows.users = append(rows.users, fakeUser{id: "u-2", username: "other.person"})
		}
		return rows, nil
	case strings.Contains(query, "disabled_at IS NOT NULL"):
		return &boolRow{value: false}, nil
	case strings.Contains(query, "INSERT INTO users"):
		c.insertCalled = true
		if len(args) > 0 {
			if name, ok := args[0].Value.(string); ok {
				c.insertedUsername = name
			}
		}
		return &userRows{
			users:   []fakeUser{{id: "new-user", username: c.insertedUsername}},
			columns: selectedColumns(query),
		}, nil
	}
	c.unmatched = append(c.unmatched, strings.Join(strings.Fields(query), " "))
	return &userRows{columns: selectedColumns(query)}, nil
}

// boolRow answers a single-column, single-row boolean query (IsUserDisabled).
type boolRow struct {
	value bool
	done  bool
}

func (r *boolRow) Columns() []string { return []string{"disabled"} }
func (r *boolRow) Close() error      { return nil }
func (r *boolRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

type userRows struct {
	users []fakeUser
	next  int
	// columns is read off the query rather than hard-coded. A fake that always
	// answers with the same column list cannot catch the one mistake this
	// fake exists near: adding a column to a SELECT and forgetting to widen
	// the Scan. database/sql compares len(dest) with len(Columns()), so
	// mirroring the query is what turns that into a test failure instead of a
	// 500 in production. It has happened; see GetUserByEmail.
	columns []string
}

// selectedColumns pulls the column list out of a SELECT, so the fake answers
// with exactly what the query asked for.
func selectedColumns(query string) []string {
	// Collapse whitespace first. These queries are raw string literals with
	// tabs and newlines, so looking for " FROM " against the original text
	// finds nothing and silently falls back — which is exactly the kind of
	// quiet default that let a column/scan mismatch reach production once.
	flat := strings.Join(strings.Fields(query), " ")
	upper := strings.ToUpper(flat)
	start := strings.Index(upper, "SELECT ")
	from := strings.Index(upper, " FROM ")
	if start < 0 || from < 0 || from < start {
		return []string{"id", "username", "is_admin", "created_at", "kind", "email"}
	}
	list := flat[start+len("SELECT ") : from]
	var columns []string
	depth, current := 0, strings.Builder{}
	flush := func() {
		name := strings.TrimSpace(current.String())
		current.Reset()
		if name == "" {
			return
		}
		// COALESCE(email, '') and u.email both name email.
		if open := strings.Index(name, "("); open >= 0 {
			name = name[open+1:]
			if comma := strings.Index(name, ","); comma >= 0 {
				name = name[:comma]
			}
			name = strings.TrimRight(name, ")")
		}
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		columns = append(columns, strings.TrimSpace(name))
	}
	for _, ch := range list {
		switch {
		case ch == '(':
			depth++
			current.WriteRune(ch)
		case ch == ')':
			depth--
			current.WriteRune(ch)
		case ch == ',' && depth == 0:
			flush()
		default:
			current.WriteRune(ch)
		}
	}
	flush()
	return columns
}

func (r *userRows) Columns() []string {
	if len(r.columns) == 0 {
		return []string{"id", "username", "is_admin", "created_at", "kind", "email"}
	}
	return r.columns
}
func (r *userRows) Close() error { return nil }
func (r *userRows) Next(dest []driver.Value) error {
	if r.next >= len(r.users) {
		return io.EOF
	}
	u := r.users[r.next]
	r.next++
	for i, column := range r.Columns() {
		if i >= len(dest) {
			break
		}
		switch column {
		case "id":
			dest[i] = u.id
		case "username":
			dest[i] = u.username
		case "is_admin":
			dest[i] = false
		case "created_at":
			dest[i] = time.Now()
		case "kind":
			dest[i] = "person"
		case "email":
			dest[i] = u.username + "@example.test"
		default:
			dest[i] = nil
		}
	}
	return nil
}
