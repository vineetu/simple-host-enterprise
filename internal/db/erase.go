package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/identityhash"
)

// A person's data, for an admin's "Export this person's data" and "Delete
// this person and all their data" (internal/handler/admin_erase.go). Both are
// offered only for a disabled person: the company, not the employee, decides
// what happens to a work account.

// ErasablePerson is the person an export or erasure acts on.
type ErasablePerson struct {
	ID       string
	Username string
	Email    string
	// EmailSource is how Email was learned ('claimed' when the identity
	// provider vouched for it at a sign-in).
	EmailSource string
	// Subject is the identity provider's subject the account signed in
	// with ('' when it never did).
	Subject  string
	Disabled bool
}

// ErrNotAPerson is returned for a team name: a team is deleted, not erased.
var ErrNotAPerson = errors.New("not a person")

// GetErasablePerson resolves a person by username. With lock set (tx only)
// the row is held for the rest of the transaction, so a concurrent enable
// cannot slip in between the disabled check and the erasure.
func GetErasablePerson(ctx context.Context, q Querier, username string, lock bool) (ErasablePerson, error) {
	query := `SELECT id::text, username, COALESCE(email, ''), COALESCE(email_source, ''), COALESCE(oidc_sub, ''), disabled_at IS NOT NULL, kind FROM users WHERE username = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var p ErasablePerson
	var kind string
	if err := q.QueryRowContext(ctx, query, username).Scan(&p.ID, &p.Username, &p.Email, &p.EmailSource, &p.Subject, &p.Disabled, &kind); err != nil {
		return p, err
	}
	if kind == "team" {
		return p, ErrNotAPerson
	}
	return p, nil
}

// QueryJSON runs a query that returns one json value (a json_agg or a
// row_to_json) and hands it back as is. An empty result is "null".
func QueryJSON(ctx context.Context, q Querier, query string, args ...any) (json.RawMessage, error) {
	var out []byte
	err := q.QueryRowContext(ctx, query, args...).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) || out == nil {
		return json.RawMessage("null"), nil
	}
	return out, err
}

// PersonExportQueries are the account-level parts of a person's export, file
// name to query, each taking the user id as $1. Metadata only: no key hash,
// no token, no client secret ever leaves.
var PersonExportQueries = []struct{ File, Query string }{
	{"account.json", `SELECT row_to_json(t) FROM (
		SELECT id, username, email, email_source, oidc_sub AS sign_in_subject, is_admin, created_at, disabled_at
		FROM users WHERE id = $1::uuid) t`},
	{"teams.json", `SELECT COALESCE(json_agg(t ORDER BY t.team), '[]') FROM (
		SELECT team.username AS team, tm.created_at AS member_since
		FROM team_members tm JOIN users team ON team.id = tm.team_id
		WHERE tm.user_id = $1::uuid) t`},
	{"viewer-grants.json", `SELECT COALESCE(json_agg(t ORDER BY t.owner, t.site), '[]') FROM (
		SELECT owner.username AS owner, s.name AS site, sv.created_at AS granted_at
		FROM site_viewers sv JOIN sites s ON s.id = sv.site_id JOIN users owner ON owner.id = s.user_id
		WHERE sv.principal_id = $1::uuid) t`},
	{"api-keys.json", `SELECT COALESCE(json_agg(t ORDER BY t.created_at), '[]') FROM (
		SELECT name, scope, created_at, expires_at, last_used_at, revoked_at
		FROM api_keys WHERE user_id = $1::uuid) t`},
	{"connected-apps.json", `SELECT COALESCE(json_agg(t ORDER BY t.connected_at), '[]') FROM (
		SELECT c.client_name AS app, g.resource, g.created_at AS connected_at, g.last_used_at, g.device_hint AS device
		FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id
		WHERE g.user_id = $1::uuid) t`},
	{"sessions.json", `SELECT COALESCE(json_agg(t ORDER BY t.created_at), '[]') FROM (
		SELECT created_at, last_seen_at, expires_at, revoked_at, host(ip) AS ip, user_agent
		FROM sessions WHERE user_id = $1::uuid) t`},
	// Grants waiting for their provider-vouched email: the same ones an
	// erasure deletes.
	{"pending-grants.json", `WITH me AS (SELECT lower(email) AS email FROM users WHERE id = $1::uuid AND email_source = 'claimed')
		SELECT COALESCE(json_agg(t ORDER BY t.granted_at), '[]') FROM (
		SELECT 'viewer' AS kind, owner.username AS owner, s.name AS site, NULL AS team, p.created_at AS granted_at
		FROM pending_site_viewers p JOIN me ON p.email = me.email
		JOIN sites s ON s.id = p.site_id JOIN users owner ON owner.id = s.user_id
		UNION ALL
		SELECT 'team_member', NULL, NULL, team.username, p.created_at
		FROM pending_team_members p JOIN me ON p.email = me.email
		JOIN users team ON team.id = p.team_id) t`},
}

// PersonVisitsQuery is visits.jsonl: one JSON object per access-log row
// where they were the visitor (the rows an erasure deletes), newest first,
// streamed rather than aggregated.
const PersonVisitsQuery = `SELECT row_to_json(t)::text FROM (
	SELECT at, owner_label, site_name, path, method, status, host(ip) AS ip, user_agent, referrer_domain
	FROM access_log WHERE user_id = $1::uuid ORDER BY at DESC) t`

// SiteExportQueries are the per-site parts, each taking the site id as $1.
var SiteExportQueries = []struct{ File, Query string }{
	{"site.json", `SELECT row_to_json(t) FROM (
		SELECT name, access, active_version, created_at, updated_at, deleted_at
		FROM sites WHERE id = $1::uuid) t`},
	{"saved-data.json", `SELECT row_to_json(t) FROM (
		SELECT state_version, state FROM sites WHERE id = $1::uuid) t`},
	{"saved-data-history.json", `SELECT COALESCE(json_agg(t ORDER BY t.id DESC), '[]') FROM (
		SELECT id, state_version, state, created_at FROM site_state_history WHERE site_id = $1::uuid) t`},
	{"versions.json", `SELECT COALESCE(json_agg(t ORDER BY t.version DESC), '[]') FROM (
		SELECT v.version_number AS version, v.status, v.size_bytes, v.created_at, uploader.username AS uploaded_by
		FROM versions v LEFT JOIN users uploader ON uploader.id = v.uploaded_by
		WHERE v.site_id = $1::uuid) t`},
	{"assets.json", `SELECT COALESCE(json_agg(t ORDER BY t.created_at), '[]') FROM (
		SELECT id, name, content_type, size, created_at, deleted_at FROM site_assets WHERE site_id = $1::uuid) t`},
}

// OwnedSite is one site a person owns, recovery window included.
type OwnedSite struct {
	TeamSite
	Deleted bool
}

// ListAllOwnerSites returns every site ownerID holds, live or in Recently
// deleted: an export and an erasure both cover the recovery window too.
func ListAllOwnerSites(ctx context.Context, q Querier, ownerID string) ([]OwnedSite, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id::text, name, active_version, deleted_at IS NOT NULL
		FROM sites WHERE user_id = $1::uuid ORDER BY name`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list all owner sites: %w", err)
	}
	defer rows.Close()
	var sites []OwnedSite
	for rows.Next() {
		var s OwnedSite
		if err := rows.Scan(&s.ID, &s.Name, &s.ActiveVersion, &s.Deleted); err != nil {
			return nil, fmt.Errorf("list all owner sites: %w", err)
		}
		sites = append(sites, s)
	}
	return sites, rows.Err()
}

// LockPersonAndTeams row-locks (FOR UPDATE) userID's own row and every team
// they belong to, all in id order, and returns the teams they are the only
// member of. Erasing that person would empty those teams, which a team never
// is (RemoveTeamMember's rule), so erasure refuses them.
//
// These are namespace rows, taken before any site-name lock and in the same
// id order LockNamespacesShared uses, so a move into or out of any of them
// waits for the erasure rather than deadlocking with it. With the person's
// row held nobody can add them to another team (the membership insert
// waits on it), so a membership added between the list and the locks is
// caught by listing again. A team found that way whose id sorts before a
// row already held would break the order, so the locks are given back (a
// savepoint releases row locks taken after it) and taken again, the whole
// set in order.
// afterTeamsListed, when set by a test, runs after each listing of the
// person's teams, where a concurrent membership change can land.
var afterTeamsListed func(round int)

func LockPersonAndTeams(ctx context.Context, tx *sql.Tx, userID string) (lastMemberOf []string, err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT lock_person_and_teams`); err != nil {
		return nil, fmt.Errorf("lock person and teams: %w", err)
	}
	var teams []Team
	locked := map[string]bool{}
	highest := ""
	for round := 0; ; round++ {
		if round >= 10 {
			return nil, errors.New("lock person and teams: memberships kept changing")
		}
		if teams, err = ListTeamsForUser(ctx, tx, userID); err != nil {
			return nil, err
		}
		if afterTeamsListed != nil {
			afterTeamsListed(round)
		}
		ids := []string{}
		if !locked[userID] {
			ids = append(ids, userID)
		}
		for _, team := range teams {
			if !locked[team.ID] {
				ids = append(ids, team.ID)
			}
		}
		if len(ids) == 0 {
			break
		}
		sort.Strings(ids)
		if len(locked) > 0 && ids[0] < highest {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT lock_person_and_teams`); err != nil {
				return nil, fmt.Errorf("lock person and teams: %w", err)
			}
			locked, highest = map[string]bool{}, ""
			continue
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
				return nil, fmt.Errorf("lock person and teams: %w", err)
			}
			locked[id] = true
			highest = id
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT lock_person_and_teams`); err != nil {
		return nil, fmt.Errorf("lock person and teams: %w", err)
	}
	for _, team := range teams {
		var others int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM team_members WHERE team_id = $1::uuid AND user_id <> $2::uuid`,
			team.ID, userID).Scan(&others); err != nil {
			return nil, err
		}
		if others == 0 {
			lastMemberOf = append(lastMemberOf, team.Username)
		}
	}
	return lastMemberOf, nil
}

// ErasureCounts is what ErasePerson removed, for the user_erased audit row.
type ErasureCounts struct {
	AccessLogRows  int64 `json:"access_log_rows"`
	PendingGrants  int64 `json:"pending_grants"`
	APIKeys        int64 `json:"api_keys"`
	ConnectedApps  int64 `json:"connected_apps"`
	Sessions       int64 `json:"sessions"`
	ViewerGrants   int64 `json:"viewer_grants"`
	TeamMembership int64 `json:"team_memberships"`
	Redirects      int64 `json:"redirects"`
}

// ErasePerson removes everything of p's that is not a site, then the users
// row, in tx. The caller has already deleted p's sites (their saved data,
// history, versions, assets and redirects go with each row) and checked
// LockPersonAndTeams.
//
// Keys, connected apps (grants, codes and tokens), sessions (and their
// handoff codes), team memberships, viewer grants they hold and network
// approvals they gave go with the users row (ON DELETE CASCADE); the
// columns that only name them as who did something (added_by, uploaded_by,
// written_by, created_by, network_requested_by, team_audit's actor and
// subject) are set to NULL by their foreign keys. sites.deleted_by has no
// foreign key and is cleared here. Access-log rows where they were the
// visitor are deleted; audit_events rows are left alone (hash-chained).
// Their address label is held in erased_owner_labels so nobody inherits it,
// and their sign-in identity in erased_identities (hashed; issuer is
// OIDC_ISSUER) so they cannot sign straight back in. erasedBy is the admin.
func ErasePerson(ctx context.Context, tx *sql.Tx, p ErasablePerson, label, issuer, erasedBy string) (ErasureCounts, error) {
	var c ErasureCounts
	count := func(dst *int64, query string, args ...any) error {
		return tx.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	// Every label this erasure holds is locked first, so a sign-in taking
	// one waits and then finds it held (migration 0057).
	labels := []string{label}
	rows, err := tx.QueryContext(ctx, `SELECT owner_label FROM renamed_owner_labels WHERE user_id = $1::uuid`, p.ID)
	if err != nil {
		return c, fmt.Errorf("erase person: %w", err)
	}
	for rows.Next() {
		var held string
		if err := rows.Scan(&held); err != nil {
			rows.Close()
			return c, fmt.Errorf("erase person: %w", err)
		}
		labels = append(labels, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("erase person: %w", err)
	}
	if err := LockOwnerLabels(ctx, tx, labels...); err != nil {
		return c, fmt.Errorf("erase person: %w", err)
	}
	steps := []struct {
		dst   *int64
		query string
		args  []any
	}{
		{&c.APIKeys, `SELECT count(*) FROM api_keys WHERE user_id = $1::uuid`, []any{p.ID}},
		{&c.ConnectedApps, `SELECT count(*) FROM oauth_grants WHERE user_id = $1::uuid`, []any{p.ID}},
		{&c.Sessions, `SELECT count(*) FROM sessions WHERE user_id = $1::uuid`, []any{p.ID}},
		{&c.ViewerGrants, `SELECT count(*) FROM site_viewers WHERE principal_id = $1::uuid`, []any{p.ID}},
		{&c.TeamMembership, `SELECT count(*) FROM team_members WHERE user_id = $1::uuid`, []any{p.ID}},
		// Only an address the identity provider vouched for is theirs: a
		// pending grant naming an inferred address may be meant for
		// whoever really holds it.
		{&c.PendingGrants, `WITH a AS (DELETE FROM pending_site_viewers WHERE $2 = 'claimed' AND email = lower($1) RETURNING 1),
			b AS (DELETE FROM pending_team_members WHERE $2 = 'claimed' AND email = lower($1) RETURNING 1)
			SELECT (SELECT count(*) FROM a) + (SELECT count(*) FROM b)`, []any{p.Email, p.EmailSource}},
		// Addresses under their label that still redirect to a site they
		// handed on (or renamed away from and then lost): the label is
		// held, so these would send their old links to someone else's
		// site. They stop; the old address is not found. Redirects to
		// their own sites went with those sites.
		{&c.Redirects, `WITH r AS (DELETE FROM site_redirects WHERE owner_label = $1 RETURNING 1) SELECT count(*) FROM r`, []any{label}},
	}
	for _, step := range steps {
		if err := count(step.dst, step.query, step.args...); err != nil {
			return c, fmt.Errorf("erase person: %w", err)
		}
	}
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE sites SET deleted_by = NULL WHERE deleted_by = $1::uuid`, []any{p.ID}},
		// Per-person rate-limit windows are keyed by the id; they expire on
		// their own, but nothing needs to keep them.
		{`DELETE FROM rate_limit_counters WHERE strpos(key, $1) > 0`, []any{p.ID}},
		{`INSERT INTO erased_owner_labels (owner_label) VALUES ($1) ON CONFLICT DO NOTHING`, []any{label}},
		// Labels they had before an admin renamed them stay held too, and
		// stop redirecting, like their last one.
		{`DELETE FROM site_redirects WHERE owner_label IN (SELECT owner_label FROM renamed_owner_labels WHERE user_id = $1::uuid)`, []any{p.ID}},
		{`INSERT INTO erased_owner_labels (owner_label) SELECT owner_label FROM renamed_owner_labels WHERE user_id = $1::uuid ON CONFLICT DO NOTHING`, []any{p.ID}},
		{`DELETE FROM renamed_owner_labels WHERE user_id = $1::uuid`, []any{p.ID}},
	} {
		if _, err := tx.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			return c, fmt.Errorf("erase person: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1::uuid AND kind = 'person'`, p.ID)
	if err != nil {
		return c, fmt.Errorf("erase person: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return c, err
	} else if n != 1 {
		return c, sql.ErrNoRows
	}
	// After the account is gone: the function refuses a live account's id.
	if err := count(&c.AccessLogRows, `SELECT access_log_erase_visitor($1::uuid)`, p.ID); err != nil {
		return c, fmt.Errorf("erase person: %w", err)
	}
	subjectHash, emailHash := sql.NullString{}, sql.NullString{}
	if p.Subject != "" {
		subjectHash = sql.NullString{String: ErasedSubjectHash(issuer, p.Subject), Valid: true}
	}
	if p.EmailSource == "claimed" && p.Email != "" {
		emailHash = sql.NullString{String: ErasedEmailHash(p.Email), Valid: true}
	}
	if subjectHash.Valid || emailHash.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO erased_identities (subject_hash, email_hash, erased_by) VALUES ($1, $2, $3::uuid)`,
			subjectHash, emailHash, sql.NullString{String: erasedBy, Valid: erasedBy != ""}); err != nil {
			return c, fmt.Errorf("erase person: %w", err)
		}
	}
	return c, nil
}

// ErrIdentityErased refuses a sign-in whose identity an admin erased.
var ErrIdentityErased = errors.New("this identity was erased")

// ErasedSubjectHash is the keyed hash of an issuer and a subject
// (identityhash.Subject), the form erased_identities and site manifests keep
// a sign-in identity in.
func ErasedSubjectHash(issuer, subject string) string {
	return identityhash.Subject(issuer, subject)
}

// ErasedEmailHash is the keyed hash of a lower-cased email.
func ErasedEmailHash(email string) string {
	return identityhash.Email(email)
}

// IsIdentityErased reports whether a sign-in identity or its email names an
// erased person, under any configured key or the plain SHA-256 rows from
// before identity hashes were keyed.
func IsIdentityErased(ctx context.Context, q Querier, issuer, subject, email string) (bool, error) {
	var erased bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM erased_identities WHERE subject_hash = ANY($1) OR email_hash = ANY($2))`,
		pq.Array(identityhash.SubjectCandidates(issuer, subject)), pq.Array(identityhash.EmailCandidates(email))).Scan(&erased)
	return erased, err
}

// ErasedIdentity is one row of the admin's "Erased people" list. Nothing in
// it names the person: the hashes cannot be reversed.
type ErasedIdentity struct {
	ID         string    `json:"id"`
	HashPrefix string    `json:"hash_prefix"`
	ErasedAt   time.Time `json:"erased_at"`
	ErasedBy   string    `json:"erased_by,omitempty"`
}

// ListErasedIdentities returns every held identity, newest first.
func ListErasedIdentities(ctx context.Context, q Querier) ([]ErasedIdentity, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT e.id::text, left(right(COALESCE(e.subject_hash, e.email_hash), 64), 12), e.erased_at, COALESCE(u.username, '')
		FROM erased_identities e LEFT JOIN users u ON u.id = e.erased_by
		ORDER BY e.erased_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list erased identities: %w", err)
	}
	defer rows.Close()
	out := []ErasedIdentity{}
	for rows.Next() {
		var e ErasedIdentity
		if err := rows.Scan(&e.ID, &e.HashPrefix, &e.ErasedAt, &e.ErasedBy); err != nil {
			return nil, fmt.Errorf("list erased identities: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllowErasedIdentity deletes one held identity, so that person can sign in
// again (to a new account). sql.ErrNoRows when id holds nothing.
func AllowErasedIdentity(ctx context.Context, q Querier, id string) (ErasedIdentity, error) {
	var e ErasedIdentity
	err := q.QueryRowContext(ctx, `
		DELETE FROM erased_identities WHERE id::text = $1
		RETURNING id::text, left(right(COALESCE(subject_hash, email_hash), 64), 12), erased_at`, id).Scan(&e.ID, &e.HashPrefix, &e.ErasedAt)
	return e, err
}
