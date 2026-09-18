package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// HandoffCodeTTL is the one-time hand-off code's lifetime (design.md 6.1):
// "mint one-time code bound to (session_id, target host, n), 60s, single use."
const HandoffCodeTTL = 60 * time.Second

// ErrHandoffCodeInvalid covers every reason a code may not be redeemed: not
// found, already redeemed, expired, or minted for a different host. One
// error for all of them, the same reasoning ErrSessionInvalid documents —
// the caller's response is identical either way, and distinguishing them
// would only help an attacker probe which reason applies.
var ErrHandoffCodeInvalid = errors.New("hand-off code is not valid")

// CreateHandoffCode inserts a new one-time code row. code is the random,
// URL-safe token minted by the caller; nonceHash is sha256 of the
// __Host-sh_handoff cookie value set on targetHost.
func CreateHandoffCode(ctx context.Context, q Querier, code, sessionID, targetHost string, nonceHash []byte) error {
	const query = `
		INSERT INTO handoff_codes (code, session_id, target_host, nonce_hash)
		VALUES ($1, $2, $3, $4)
	`
	_, err := q.ExecContext(ctx, query, code, sessionID, targetHost, nonceHash)
	return err
}

// RedeemHandoffCode atomically claims a code: it must exist, be unredeemed,
// be no older than HandoffCodeTTL, and its target_host and nonce_hash must
// match what the caller presents (the host the request arrived on, and
// sha256 of the __Host-sh_handoff cookie it is holding). On success it
// returns the session id the code was minted for and marks the row redeemed
// in the same statement, so a second, concurrent redemption attempt always
// loses.
func RedeemHandoffCode(ctx context.Context, db *sql.DB, code, targetHost string, nonceHash []byte) (sessionID string, err error) {
	const query = `
		UPDATE handoff_codes
		SET redeemed_at = now()
		WHERE code = $1
		  AND redeemed_at IS NULL
		  AND created_at > now() - make_interval(secs => $2)
		  AND target_host = $3
		  AND nonce_hash = $4
		RETURNING session_id::text
	`
	err = db.QueryRowContext(ctx, query, code, HandoffCodeTTL.Seconds(), targetHost, nonceHash).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrHandoffCodeInvalid
	}
	return sessionID, err
}
