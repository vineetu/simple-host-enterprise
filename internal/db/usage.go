package db

import (
	"context"
	"database/sql"
	"time"
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

// RecordAIUsage increments the per-owner-per-site-per-day AI usage counters
// (requests +1, token counters += the given amounts). The day boundary is UTC.
func RecordAIUsage(ctx context.Context, db *sql.DB, userID, siteName string, day time.Time, inputTokens, outputTokens int64) error {
	const query = `
		INSERT INTO ai_usage (user_id, site_name, day, requests, input_tokens, output_tokens)
		VALUES ($1, $2, $3, 1, $4, $5)
		ON CONFLICT (user_id, site_name, day) DO UPDATE SET
			requests      = ai_usage.requests + 1,
			input_tokens  = ai_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens = ai_usage.output_tokens + EXCLUDED.output_tokens
	`

	_, err := db.ExecContext(ctx, query, userID, siteName, day.UTC().Format("2006-01-02"), inputTokens, outputTokens)
	return err
}
