package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// hashHandoffCode is what handoff_codes.code holds: the hex SHA-256 of the
// code, never the code itself, so a read of the table (a backup, a
// replica, a log of the row) yields nothing redeemable.
func hashHandoffCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CreateHandoffCode inserts a new one-time code row. code is the random,
// URL-safe token minted by the caller (stored only as its hash); nonceHash
// is sha256 of the __Host-sh_handoff cookie value set on targetHost.
func CreateHandoffCode(ctx context.Context, q Querier, code, sessionID, targetHost string, nonceHash []byte) error {
	const query = `
		INSERT INTO handoff_codes (code, session_id, target_host, nonce_hash)
		VALUES ($1, $2, $3, $4)
	`
	_, err := q.ExecContext(ctx, query, hashHandoffCode(code), sessionID, targetHost, nonceHash)
	return err
}

// RedeemHandoffCode claims a code in one statement: the row is marked
// redeemed whether or not the attempt succeeds, and only then are its age,
// target_host and nonce_hash compared with what the caller presents. A code
// is therefore spent by the first attempt to use it, so a code delivered to
// a browser that cannot redeem it (no nonce cookie, wrong host) can never
// be carried off and redeemed somewhere else afterwards. nonceHash may be
// nil when the caller has no nonce cookie at all: the code is still spent.
func RedeemHandoffCode(ctx context.Context, db *sql.DB, code, targetHost string, nonceHash []byte) (sessionID string, err error) {
	const query = `
		UPDATE handoff_codes
		SET redeemed_at = now()
		WHERE code = $1
		  AND redeemed_at IS NULL
		RETURNING session_id::text,
		          created_at > now() - make_interval(secs => $2)
		          AND target_host = $3
		          AND nonce_hash = $4
	`
	var valid sql.NullBool
	err = db.QueryRowContext(ctx, query, hashHandoffCode(code), HandoffCodeTTL.Seconds(), targetHost, nonceHash).Scan(&sessionID, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrHandoffCodeInvalid
	}
	if err != nil {
		return "", err
	}
	if !valid.Valid || !valid.Bool {
		return "", ErrHandoffCodeInvalid
	}
	return sessionID, nil
}
