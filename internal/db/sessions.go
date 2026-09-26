package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is one row of the sessions table: one per sign-in, shared by
// every host cookie minted from it. Revoking it, or disabling the owning
// user, signs the person out everywhere within the cache window.
type Session struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
	RevokedAt  *time.Time
}

// CreateSession inserts a new session row and returns its id.
func CreateSession(ctx context.Context, q Querier, userID string, expiresAt time.Time, ip, userAgent string) (Session, error) {
	const query = `
		INSERT INTO sessions (user_id, expires_at, ip, user_agent)
		VALUES ($1, $2, NULLIF($3, '')::inet, $4)
		RETURNING id, user_id, created_at, expires_at, last_seen_at, COALESCE(host(ip), ''), user_agent, revoked_at
	`
	var s Session
	err := q.QueryRowContext(ctx, query, userID, expiresAt, ip, userAgent).Scan(
		&s.ID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &s.IP, &s.UserAgent, &s.RevokedAt,
	)
	return s, err
}

// ErrSessionInvalid is returned by GetValidSession when a session id does not
// resolve to a session that is still usable: not found, expired, revoked, or
// belonging to a disabled user. Deliberately one error for all of those —
// the caller's response (treat the cookie as unauthenticated) is the same in
// every case, and distinguishing them would only help an attacker probe
// which reason applies.
var ErrSessionInvalid = errors.New("session is not valid")

// sessionWithUser carries the joined user alongside the session, since every
// caller of GetValidSession needs both: the session to validate the cookie
// and the user to authenticate the request as.
type SessionWithUser struct {
	Session Session
	User    User
}

// GetValidSession loads a session by id together with its owning user, and
// returns ErrSessionInvalid unless: the session exists, is not revoked, has
// not expired, has not sat idle past idleTimeout since it was last seen, and
// the owning user is not disabled. idleTimeout <= 0 disables the idle check.
func GetValidSession(ctx context.Context, db *sql.DB, sessionID string, idleTimeout time.Duration) (SessionWithUser, error) {
	const query = `
		SELECT s.id, s.user_id, s.created_at, s.expires_at, s.last_seen_at, COALESCE(host(s.ip), ''), s.user_agent, s.revoked_at,
		       u.id, u.username, u.is_admin, u.created_at, u.kind, COALESCE(u.email, ''), u.disabled_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = $1
	`
	var sess Session
	var user User
	var disabledAt *time.Time
	err := db.QueryRowContext(ctx, query, sessionID).Scan(
		&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt, &sess.IP, &sess.UserAgent, &sess.RevokedAt,
		&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind, &user.Email, &disabledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionWithUser{}, ErrSessionInvalid
	}
	if err != nil {
		return SessionWithUser{}, err
	}
	if sess.RevokedAt != nil || disabledAt != nil {
		return SessionWithUser{}, ErrSessionInvalid
	}
	now := time.Now()
	if now.After(sess.ExpiresAt) {
		return SessionWithUser{}, ErrSessionInvalid
	}
	if idleTimeout > 0 && now.After(sess.LastSeenAt.Add(idleTimeout)) {
		return SessionWithUser{}, ErrSessionInvalid
	}
	return SessionWithUser{Session: sess, User: user}, nil
}

// touchSessionInterval bounds how often TouchSession actually writes: the
// serving path calls it on every authenticated control-plane request, and a
// write every request would put every page view a write lock away from a
// slow session table, so writes happen at most once per five minutes.
const touchSessionInterval = 5 * time.Minute

// TouchSession updates last_seen_at, but only if it has not been touched
// within touchSessionInterval — a no-op write costs one UPDATE that matches
// zero rows, which is what keeps this cheap enough to call unconditionally.
func TouchSession(ctx context.Context, db *sql.DB, sessionID string) error {
	const query = `
		UPDATE sessions
		SET last_seen_at = now()
		WHERE id = $1 AND last_seen_at < now() - make_interval(secs => $2)
	`
	_, err := db.ExecContext(ctx, query, sessionID, touchSessionInterval.Seconds())
	return err
}

// ListSessionsForUser returns a user's sessions, newest first, for the
// /auth/sessions dashboard. Revoked sessions are included so a person can
// see what they signed out of, not only what is still live.
func ListSessionsForUser(ctx context.Context, db *sql.DB, userID string) ([]Session, error) {
	const query = `
		SELECT id, user_id, created_at, expires_at, last_seen_at, COALESCE(host(ip), ''), user_agent, revoked_at
		FROM sessions
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &s.IP, &s.UserAgent, &s.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSession revokes one session, scoped to userID so a person can only
// revoke their own. Returns sql.ErrNoRows if it doesn't exist, isn't theirs,
// or is already revoked.
func RevokeSession(ctx context.Context, db Querier, userID, sessionID string) error {
	const query = `
		UPDATE sessions SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`
	result, err := db.ExecContext(ctx, query, sessionID, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const listBlockedSessionIDsQuery = `
	SELECT s.id::text
	FROM sessions s
	JOIN users u ON u.id = s.user_id
	WHERE s.revoked_at IS NOT NULL
	   OR s.expires_at < now()
	   OR s.last_seen_at < now() - make_interval(secs => $1)
	   OR u.disabled_at IS NOT NULL
`

// ListBlockedSessionIDs returns every session id that must be treated as
// invalid on the hosted-content path: revoked, expired, idle past
// idleTimeout, or belonging to a disabled user. This is the negative cache
// source: hosted content verifies the signed cookie and consults an
// in-memory set built from this query every 60 seconds, rather than
// reading the sessions table on every page view or asset request.
func ListBlockedSessionIDs(ctx context.Context, db *sql.DB, idleTimeout time.Duration) ([]string, error) {
	rows, err := db.QueryContext(ctx, listBlockedSessionIDsQuery, idleTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RevokeAllSessionsForUser revokes every live session for a user: called
// on sign-out-everywhere and on offboarding. Runs against the given
// Querier so a caller can fold it into the same transaction as disabling
// the account.
func RevokeAllSessionsForUser(ctx context.Context, q Querier, userID string) error {
	const query = `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := q.ExecContext(ctx, query, userID)
	return err
}

// PruneSessions deletes hand-off codes older than a day (useful for 60
// seconds) and sessions that expired or were revoked more than
// retentionDays ago, so their ip and user_agent do not outlive the access
// log's retention. Run by the prune job as the owning role. It returns the
// number of sessions deleted.
func PruneSessions(ctx context.Context, q Querier, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, errors.New("PruneSessions requires positive retention days")
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM handoff_codes WHERE created_at < now() - interval '1 day'`); err != nil {
		return 0, err
	}
	result, err := q.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE expires_at < now() - make_interval(days => $1)
		   OR revoked_at < now() - make_interval(days => $1)`, retentionDays)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
