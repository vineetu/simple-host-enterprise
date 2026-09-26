package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// OAuthClient is one registered app (RFC 7591). SecretHash is nil for a
// public client (token_endpoint_auth_method "none").
type OAuthClient struct {
	ClientID                string
	SecretHash              []byte
	Name                    string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
}

// OAuthCode is a stored authorization code, looked up by its hash.
type OAuthCode struct {
	ClientID      string
	UserID        string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
	GrantID       sql.NullString
}

// OAuthToken is a stored access or refresh token with its grant.
type OAuthToken struct {
	GrantID   string
	Kind      string
	ExpiresAt time.Time
	UsedAt    *time.Time
	UserID    string
	ClientID  string
	Resource  string
	// GrantCreatedAt is when the person allowed the app: the start of the
	// connection, which the refresh lifetime is measured from.
	GrantCreatedAt time.Time
}

// ErrOAuthCodeUsed is returned by ConsumeOAuthCode for a code already
// redeemed; the returned OAuthCode still carries its GrantID.
var ErrOAuthCodeUsed = errors.New("authorization code already used")

func InsertOAuthClient(ctx context.Context, q Querier, c OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	var secretHash any // a nil []byte would be stored as an empty bytea, not NULL
	if c.SecretHash != nil {
		secretHash = c.SecretHash
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO oauth_clients (client_id, client_secret_hash, client_name, redirect_uris, token_endpoint_auth_method)
		VALUES ($1, $2, $3, $4, $5)`,
		c.ClientID, secretHash, c.Name, string(uris), c.TokenEndpointAuthMethod)
	return err
}

func GetOAuthClient(ctx context.Context, q Querier, clientID string) (OAuthClient, error) {
	var c OAuthClient
	var uris string
	err := q.QueryRowContext(ctx, `
		SELECT client_id, client_secret_hash, client_name, redirect_uris::text, token_endpoint_auth_method
		FROM oauth_clients WHERE client_id = $1`, clientID).
		Scan(&c.ClientID, &c.SecretHash, &c.Name, &uris, &c.TokenEndpointAuthMethod)
	if err != nil {
		return OAuthClient{}, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return OAuthClient{}, err
	}
	return c, nil
}

func TouchOAuthClient(ctx context.Context, q Querier, clientID string) error {
	_, err := q.ExecContext(ctx, `UPDATE oauth_clients SET last_used_at = now() WHERE client_id = $1`, clientID)
	return err
}

func InsertOAuthCode(ctx context.Context, q Querier, codeHash []byte, c OAuthCode) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, resource, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		codeHash, c.ClientID, c.UserID, c.RedirectURI, c.CodeChallenge, c.Resource, c.ExpiresAt)
	return err
}

// ConsumeOAuthCode marks a code used and returns it. A code already used
// returns ErrOAuthCodeUsed with the stored row, so the caller can revoke
// the grant the first redemption created.
func ConsumeOAuthCode(ctx context.Context, tx *sql.Tx, codeHash []byte) (OAuthCode, error) {
	var c OAuthCode
	var usedAt *time.Time
	err := tx.QueryRowContext(ctx, `
		SELECT client_id, user_id, redirect_uri, code_challenge, resource, expires_at, used_at, grant_id
		FROM oauth_codes WHERE code_hash = $1 FOR UPDATE`, codeHash).
		Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Resource, &c.ExpiresAt, &usedAt, &c.GrantID)
	if err != nil {
		return OAuthCode{}, err
	}
	if usedAt != nil {
		return c, ErrOAuthCodeUsed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET used_at = now() WHERE code_hash = $1`, codeHash); err != nil {
		return OAuthCode{}, err
	}
	return c, nil
}

func SetOAuthCodeGrant(ctx context.Context, tx *sql.Tx, codeHash []byte, grantID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET grant_id = $2 WHERE code_hash = $1`, codeHash, grantID)
	return err
}

// InsertOAuthGrant records a new connection and returns its id and
// created_at, the start its refresh lifetime is measured from.
func InsertOAuthGrant(ctx context.Context, tx *sql.Tx, userID, clientID, resource string) (string, time.Time, error) {
	var id string
	var createdAt time.Time
	err := tx.QueryRowContext(ctx, `
		INSERT INTO oauth_grants (user_id, client_id, resource) VALUES ($1, $2, $3) RETURNING id, created_at`,
		userID, clientID, resource).Scan(&id, &createdAt)
	return id, createdAt, err
}

func TouchOAuthGrant(ctx context.Context, q Querier, grantID string) error {
	_, err := q.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = now() WHERE id = $1`, grantID)
	return err
}

// DeleteOAuthGrant revokes a grant and, by cascade, every token in it.
func DeleteOAuthGrant(ctx context.Context, q Querier, grantID string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id = $1`, grantID)
	return err
}

// DeleteOAuthGrantsForUser revokes every connected app a person has; called
// when the person is disabled.
func DeleteOAuthGrantsForUser(ctx context.Context, q Querier, userID string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id = $1`, userID)
	return err
}

func InsertOAuthToken(ctx context.Context, tx *sql.Tx, tokenHash []byte, grantID, kind string, expiresAt time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO oauth_tokens (token_hash, grant_id, kind, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash, grantID, kind, expiresAt)
	return err
}

// GetOAuthToken looks up a token by hash, locking it when q is a
// transaction that goes on to rotate it.
func GetOAuthToken(ctx context.Context, q Querier, tokenHash []byte, forUpdate bool) (OAuthToken, error) {
	query := `
		SELECT t.grant_id, t.kind, t.expires_at, t.used_at, g.user_id, g.client_id, g.resource, g.created_at
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.token_hash = $1`
	if forUpdate {
		query += ` FOR UPDATE OF t`
	}
	var t OAuthToken
	err := q.QueryRowContext(ctx, query, tokenHash).
		Scan(&t.GrantID, &t.Kind, &t.ExpiresAt, &t.UsedAt, &t.UserID, &t.ClientID, &t.Resource, &t.GrantCreatedAt)
	return t, err
}

func MarkOAuthTokenUsed(ctx context.Context, tx *sql.Tx, tokenHash []byte) error {
	_, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET used_at = now() WHERE token_hash = $1`, tokenHash)
	return err
}

func DeleteOAuthToken(ctx context.Context, q Querier, tokenHash []byte) error {
	_, err := q.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE token_hash = $1`, tokenHash)
	return err
}

// GetUserByOAuthAccessToken is the Bearer hot path: an unexpired access
// token of a person who is not disabled. Anything else is sql.ErrNoRows.
func GetUserByOAuthAccessToken(ctx context.Context, q Querier, tokenHash []byte) (User, string, error) {
	const query = `
		SELECT u.id, u.username, u.is_admin, u.created_at, u.kind, COALESCE(u.email, ''), g.id
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.id = t.grant_id
		JOIN users u ON u.id = g.user_id
		WHERE t.token_hash = $1 AND t.kind = 'access' AND t.expires_at > now()
		  AND u.disabled_at IS NULL AND u.kind = 'person'
	`
	var user User
	var grantID string
	err := q.QueryRowContext(ctx, query, tokenHash).Scan(
		&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind, &user.Email, &grantID,
	)
	if err != nil {
		return User{}, "", err
	}
	return user, grantID, nil
}

// SweepOAuth deletes what can no longer be used: expired codes and
// tokens, grants left with no token, and registrations that never
// reached the token endpoint within a day.
func SweepOAuth(ctx context.Context, q Querier) error {
	for _, stmt := range []string{
		`DELETE FROM oauth_codes WHERE expires_at < now() - interval '1 hour'`,
		`DELETE FROM oauth_tokens WHERE expires_at < now()`,
		`DELETE FROM oauth_grants g WHERE g.created_at < now() - interval '1 hour'
		   AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.grant_id = g.id)`,
		`DELETE FROM oauth_clients c WHERE c.created_at < now() - interval '1 day'
		   AND c.last_used_at IS NULL AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = c.client_id)`,
	} {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
