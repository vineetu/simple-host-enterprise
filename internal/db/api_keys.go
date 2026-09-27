package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

// APIKey is one row of the api_keys table: a named, individually revocable
// credential for X-API-Key. The plaintext is returned once, at mint time,
// and never stored — key_hash (SHA-256 of the 32 random plaintext bytes)
// is all this row ever holds.
type APIKey struct {
	ID     string
	UserID string
	Name   string
	Prefix string
	// Last4 is the key's own last four characters (migration 0050), ""
	// for a key minted before it was kept.
	Last4      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	ExpiresAt  time.Time
	// Scope is what the key may call (migration 0035): APIKeyScopePublish,
	// APIKeyScopeFull or APIKeyScopeOffboard. internal/auth enforces it.
	Scope string
}

// The scopes an API key can carry (api_keys_scope_check in 0035).
const (
	APIKeyScopePublish  = "publish"
	APIKeyScopeFull     = "full"
	APIKeyScopeOffboard = "offboard"
)

// HashAPIKey returns the SHA-256 of a plaintext key, the form stored in
// key_hash and looked up on every X-API-Key request.
func HashAPIKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// KeyPrefix returns the first 8 hex characters of a key's hash, for display
// only — it grants nothing on its own, and exists so a list of keys is
// recognizable to the person who holds them.
func KeyPrefix(hash []byte) string {
	const n = 4 // 4 bytes = 8 hex characters
	if len(hash) < n {
		return ""
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, b := range hash[:n] {
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

// CreateAPIKey inserts a new key row. keyHash/prefix come from HashAPIKey /
// KeyPrefix on the freshly generated plaintext, which the caller returns to
// its client once and never persists.
func CreateAPIKey(ctx context.Context, q Querier, userID, name string, keyHash []byte, prefix, last4 string, expiresAt time.Time, scope string) (APIKey, error) {
	const query = `
		INSERT INTO api_keys (user_id, name, key_hash, prefix, last4, expires_at, scope)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7)
		RETURNING id, user_id, name, prefix, COALESCE(last4, ''), created_at, last_used_at, revoked_at, expires_at, scope
	`
	var k APIKey
	err := q.QueryRowContext(ctx, query, userID, name, keyHash, prefix, last4, expiresAt, scope).Scan(
		&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Last4, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.Scope,
	)
	return k, err
}

// LiveAPIKey is what the X-API-Key hot path needs of the key it matched.
type LiveAPIKey struct {
	ID        string
	Scope     string
	ExpiresAt time.Time
}

// GetUserByAPIKeyHash looks up the (unrevoked, unexpired) key by its hash and returns
// the owning user and the key's id, scope and expiry. This is the X-API-Key hot
// path: one indexed lookup on key_hash's unique constraint, one join to users.
func GetUserByAPIKeyHash(ctx context.Context, db *sql.DB, keyHash []byte) (User, LiveAPIKey, error) {
	const query = `
		SELECT u.id, u.username, u.is_admin, u.created_at, u.kind, COALESCE(u.email, ''), u.disabled_at,
		       k.id, k.scope, k.expires_at
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND k.expires_at > now()
	`
	var user User
	var disabledAt *time.Time
	var key LiveAPIKey
	err := db.QueryRowContext(ctx, query, keyHash).Scan(
		&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind, &user.Email, &disabledAt,
		&key.ID, &key.Scope, &key.ExpiresAt,
	)
	if err != nil {
		return User{}, LiveAPIKey{}, err
	}
	if disabledAt != nil {
		return User{}, LiveAPIKey{}, sql.ErrNoRows
	}
	return user, key, nil
}

// touchAPIKeyInterval mirrors touchSessionInterval: last_used_at is display
// and audit data, not a control, so it is written at most this often rather
// than on every request.
const touchAPIKeyInterval = 5 * time.Minute

// TouchAPIKey updates last_used_at, throttled the same way TouchSession is.
func TouchAPIKey(ctx context.Context, db *sql.DB, keyID string) error {
	const query = `
		UPDATE api_keys
		SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - make_interval(secs => $2))
	`
	_, err := db.ExecContext(ctx, query, keyID, touchAPIKeyInterval.Seconds())
	return err
}

// ListAPIKeysForUser returns a user's keys, newest first, for the /api/keys
// list and the dashboard's keys page. Revoked keys are included so a person
// can see what they turned off.
func ListAPIKeysForUser(ctx context.Context, db *sql.DB, userID string) ([]APIKey, error) {
	const query = `
		SELECT id, user_id, name, prefix, COALESCE(last4, ''), created_at, last_used_at, revoked_at, expires_at, scope
		FROM api_keys
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Last4, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt, &k.ExpiresAt, &k.Scope); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes one key, scoped to userID so a person can only revoke
// their own. Returns sql.ErrNoRows if it doesn't exist, isn't theirs, or is
// already revoked.
func RevokeAPIKey(ctx context.Context, db Querier, userID, keyID string) error {
	const query = `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`
	result, err := db.ExecContext(ctx, query, keyID, userID)
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

// RevokeAllAPIKeysForUser revokes every live key for a user, for the same
// offboarding path RevokeAllSessionsForUser serves.
func RevokeAllAPIKeysForUser(ctx context.Context, q Querier, userID string) error {
	const query = `UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := q.ExecContext(ctx, query, userID)
	return err
}

// ErrAPIKeyNameRequired is returned by callers that reject an empty key name
// before it ever reaches the database (kept here so the handler and any
// future caller share one message).
var ErrAPIKeyNameRequired = errors.New("api key name is required")

// APIKeyRecord is one key found by its hash whatever its state, with its
// owner's name: what the refusal of a key that stopped working explains,
// and what an admin revoking a leaked key acts on.
type APIKeyRecord struct {
	APIKey
	Owner         string
	OwnerDisabled bool
}

// GetAPIKeyByHash returns the key with this hash, live or not, with its
// owner. sql.ErrNoRows when no key ever had it. Never on the hot path: only
// after GetUserByAPIKeyHash has refused a key, and for the admin's revoke.
func GetAPIKeyByHash(ctx context.Context, q Querier, keyHash []byte) (APIKeyRecord, error) {
	const query = `
		SELECT k.id, k.user_id, k.name, k.prefix, COALESCE(k.last4, ''), k.created_at, k.last_used_at,
		       k.revoked_at, k.expires_at, k.scope, u.username, u.disabled_at IS NOT NULL
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1
	`
	var r APIKeyRecord
	err := q.QueryRowContext(ctx, query, keyHash).Scan(
		&r.ID, &r.UserID, &r.Name, &r.Prefix, &r.Last4, &r.CreatedAt, &r.LastUsedAt,
		&r.RevokedAt, &r.ExpiresAt, &r.Scope, &r.Owner, &r.OwnerDisabled,
	)
	return r, err
}

// RevokeAPIKeyByID revokes one key whoever holds it, for an admin revoking
// a leaked key. sql.ErrNoRows when it is already revoked (or gone).
func RevokeAPIKeyByID(ctx context.Context, q Querier, keyID string) error {
	result, err := q.ExecContext(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
