package db

import (
	"context"
	"time"
)

// UseSiteExportLink records that the whole-site download link with this
// signature has started its download, so it works once. It reports false
// when the link was already used or has expired by the database's clock.
// Markers past their expiry are dropped on the way; a link is refused at
// insert once its expiry has passed, so dropping its marker can never make
// it usable again.
func UseSiteExportLink(ctx context.Context, q Querier, signature string, expires time.Time) (bool, error) {
	if _, err := q.ExecContext(ctx, `DELETE FROM site_export_links_used WHERE expires_at < now()`); err != nil {
		return false, err
	}
	result, err := q.ExecContext(ctx, `
		INSERT INTO site_export_links_used (signature, expires_at)
		SELECT $1, $2::timestamptz WHERE $2::timestamptz > now()
		ON CONFLICT (signature) DO NOTHING`, signature, expires)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// Credential kinds a download link records as the one that minted it.
const (
	CredentialSession = "session"
	CredentialKey     = "key"
	CredentialApp     = "app"
)

// CredentialLive reports whether the credential that minted a download link
// still works for userID: a session not revoked or expired, an API key not
// revoked or expired, or a connected app not disconnected. Revoking it (or
// signing out everywhere, or an admin disabling the person) stops the link.
func CredentialLive(ctx context.Context, q Querier, kind, id, userID string) (bool, error) {
	var query string
	switch kind {
	case CredentialSession:
		query = `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL AND expires_at > now())`
	case CredentialKey:
		query = `SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL AND expires_at > now())`
	case CredentialApp:
		query = `SELECT EXISTS (SELECT 1 FROM oauth_grants WHERE id = $1::uuid AND user_id = $2::uuid)`
	default:
		return false, nil
	}
	if id == "" {
		return false, nil
	}
	var live bool
	err := q.QueryRowContext(ctx, query, id, userID).Scan(&live)
	return live, err
}
