package db

import (
	"context"
	"database/sql"
)

// SiteOwner returns the ID of the user who owns the named site. It returns
// sql.ErrNoRows when either the user or the site does not exist.
func SiteOwner(ctx context.Context, db *sql.DB, username, siteName string) (string, error) {
	const query = `
		SELECT u.id
		FROM users u
		JOIN sites s ON s.user_id = u.id
		WHERE u.username = $1 AND s.name = $2
	`

	var userID string
	err := db.QueryRowContext(ctx, query, username, siteName).Scan(&userID)
	return userID, err
}
