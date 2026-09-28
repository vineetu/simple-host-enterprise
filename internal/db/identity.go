package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

// GetUserByOIDCSub finds the account already bound to a provider subject.
// This is the fast, primary path on every sign-in after the first: once a
// row carries oidc_sub, it is found here and email is never used to find it
// again (RefreshUserEmail only keeps the stored address current).
func GetUserByOIDCSub(ctx context.Context, db *sql.DB, sub string) (User, error) {
	const query = `
		SELECT id, username, is_admin, created_at, kind, COALESCE(email, '')
		FROM users
		WHERE oidc_sub = $1
	`
	var user User
	err := db.QueryRowContext(ctx, query, sub).Scan(
		&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind, &user.Email,
	)
	return user, err
}

// BindOIDCSub claims an existing, not-yet-bound account by writing its
// oidc_sub. Scoped to id = $1 AND oidc_sub IS NULL, so a second sign-in
// racing the first either loses (sql.ErrNoRows: fall back to re-reading by
// sub, which the racer's own write just satisfied) or the row was already
// somebody else's binding target, which the caller must not silently
// overwrite either way: binding happens only once.
func BindOIDCSub(ctx context.Context, db *sql.DB, userID, sub string) error {
	const query = `UPDATE users SET oidc_sub = $2 WHERE id = $1 AND oidc_sub IS NULL`
	result, err := db.ExecContext(ctx, query, userID, sub)
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

// CreateOIDCUser creates a brand-new person, bound to sub from the moment it
// exists. They mint their first api_keys row from the dashboard, same as
// anyone else — there is no legacy plaintext key to leave NULL any more.
func CreateOIDCUser(ctx context.Context, db *sql.DB, username, sub, email string, isAdmin bool) (User, error) {
	const query = `
		INSERT INTO users (username, is_admin, oidc_sub, email, email_source)
		VALUES ($1, $2, $3, NULLIF($4, ''), CASE WHEN $4 = '' THEN NULL ELSE 'claimed' END)
		RETURNING id, username, is_admin, created_at, kind, COALESCE(email, '')
	`
	var user User
	err := db.QueryRowContext(ctx, query, username, isAdmin, sub, email).Scan(
		&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind, &user.Email,
	)
	return user, err
}

// RefreshAdminStatus sets is_admin from the outcome of the current sign-in's
// claim check: admin status is refreshed at every sign-in from the
// provider, never edited by hand — so a person removed from ADMIN_EMAILS
// loses admin on their next sign-in, without anyone touching their row.
func RefreshAdminStatus(ctx context.Context, db *sql.DB, userID string, isAdmin bool) error {
	const query = `UPDATE users SET is_admin = $2 WHERE id = $1`
	_, err := db.ExecContext(ctx, query, userID, isAdmin)
	return err
}

// EmailRefresh is the outcome of RefreshUserEmail.
type EmailRefresh struct {
	// Previous is the address the account had ("" for none); Changed
	// reports that it now holds the new one.
	Previous string
	Changed  bool
	// HeldBy is the username of another person account that already holds
	// the new address, when that stopped the change.
	HeldBy string
}

// RefreshUserEmail keeps a person's stored email in step with the address
// their identity provider verified at this sign-in (the account itself is
// found by subject, never by email), so offboarding by email matches the
// directory's current address. Addresses are not unique by design
// (migration 0017), but two people holding one address would make
// offboarding by it disable both: when another person account already
// holds the new address (compared case-insensitively) the change is
// skipped and reported in HeldBy. A transaction-scoped advisory lock on
// the address serializes two sign-ins claiming it at once.
func RefreshUserEmail(ctx context.Context, database *sql.DB, userID, email string) (EmailRefresh, error) {
	var out EmailRefresh
	if email == "" {
		return out, nil
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(email, '') FROM users WHERE id = $1 AND kind = 'person'`, userID).Scan(&out.Previous); err != nil {
		return out, err
	}
	if out.Previous == email {
		return out, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('users.email:' || lower($1)))`, email); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT username FROM users
		WHERE id <> $1 AND kind = 'person' AND email IS NOT NULL AND lower(email) = lower($2)
		ORDER BY username LIMIT 1`, userID, email).Scan(&out.HeldBy)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email = $2, email_source = 'claimed' WHERE id = $1 AND kind = 'person'`, userID, email); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Changed = true
	return out, nil
}

// ErrAccountDisabled is returned by sign-in resolution when the matched
// account has been disabled by an admin. The provider may
// still be willing to authenticate the person; this application refuses
// regardless.
var ErrAccountDisabled = errors.New("account is disabled")

// ErrLastAdmin refuses disabling the only enabled admin: nobody would be
// left who could re-enable anyone.
var ErrLastAdmin = errors.New("cannot disable the last enabled admin")

// SyncAdminEmails sets is_admin on every person to whether their email is in
// adminEmails (lowercased). Run at startup so removing someone from
// ADMIN_EMAILS takes effect on the next deploy rather than on their next
// sign-in; only valid when ADMIN_EMAILS is the sole source of admin status.
// An empty list changes nothing: an install whose ADMIN_EMAILS was left
// blank (or lost) keeps the admins it has instead of demoting every one of
// them, and the caller warns that no admin source is configured.
func SyncAdminEmails(ctx context.Context, db *sql.DB, adminEmails []string) (changed int64, err error) {
	if len(adminEmails) == 0 {
		return 0, nil
	}
	const query = `
		UPDATE users
		SET is_admin = COALESCE(lower(email) = ANY($1), false)
		WHERE kind = 'person'
		  AND is_admin IS DISTINCT FROM COALESCE(lower(email) = ANY($1), false)
	`
	result, err := db.ExecContext(ctx, query, pq.Array(adminEmails))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// IsUserDisabled reports whether a user's disabled_at is set.
func IsUserDisabled(ctx context.Context, db *sql.DB, userID string) (bool, error) {
	const query = `SELECT disabled_at IS NOT NULL FROM users WHERE id = $1`
	var disabled bool
	err := db.QueryRowContext(ctx, query, userID).Scan(&disabled)
	return disabled, err
}

// SetUserDisabled sets or clears disabled_at. Disabling also revokes every
// session, API key and connected app in the same transaction, so the
// takedown is atomic: a request already in flight when this commits either
// sees the old, valid credential (before commit) or a revoked one (after),
// never a disabled account with a still-live session.
func SetUserDisabled(ctx context.Context, database *sql.DB, userID string, disabled bool) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := SetUserDisabledTx(ctx, tx, userID, disabled); err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserDisabledTx is SetUserDisabled inside the caller's transaction, so
// the caller can write its audit row in the same commit.
func SetUserDisabledTx(ctx context.Context, tx *sql.Tx, userID string, disabled bool) error {
	if disabled {
		// Lock every enabled admin row first, so two admins disabling each
		// other concurrently cannot both see the other as the one left.
		var others int
		err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM users
				WHERE is_admin AND disabled_at IS NULL AND kind = 'person'
				FOR UPDATE
			) admins WHERE id <> $1
		`, userID).Scan(&others)
		if err != nil {
			return err
		}
		var targetIsAdmin bool
		err = tx.QueryRowContext(ctx, `SELECT is_admin AND disabled_at IS NULL FROM users WHERE id = $1`, userID).Scan(&targetIsAdmin)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if targetIsAdmin && others == 0 {
			return ErrLastAdmin
		}
	}
	var query string
	if disabled {
		query = `UPDATE users SET disabled_at = now() WHERE id = $1 AND kind = 'person'`
	} else {
		query = `UPDATE users SET disabled_at = NULL WHERE id = $1 AND kind = 'person'`
	}
	result, err := tx.ExecContext(ctx, query, userID)
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
	if disabled {
		if err := RevokeAllSessionsForUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := RevokeAllAPIKeysForUser(ctx, tx, userID); err != nil {
			return err
		}
		if err := DeleteOAuthGrantsForUser(ctx, tx, userID); err != nil {
			return err
		}
	}
	return nil
}

// PersonByEmail is one account found by PeopleByEmail.
type PersonByEmail struct {
	ID       string
	Username string
	Disabled bool
}

// PeopleByEmail returns every person whose email is email (compared
// case-insensitively), ordered by username. Offboarding
// works by address, and an address can sit on more than one account (see
// GetUserByEmail), so it answers with all of them rather than refusing.
func PeopleByEmail(ctx context.Context, q Querier, email string) ([]PersonByEmail, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, username, disabled_at IS NOT NULL
		FROM users
		WHERE kind = 'person' AND email IS NOT NULL AND lower(email) = lower($1)
		ORDER BY username`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonByEmail
	for rows.Next() {
		var p PersonByEmail
		if err := rows.Scan(&p.ID, &p.Username, &p.Disabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
