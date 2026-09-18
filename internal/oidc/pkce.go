package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// randomURLSafe returns n cryptographically random bytes, base64url-encoded
// with no padding — the shape every value below needs: a PKCE verifier, an
// OAuth state token, and an OIDC nonce are all "enough random bytes, spelled
// so they survive a URL and a cookie unescaped."
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewState returns a random state token for the OAuth authorization request.
func NewState() (string, error) { return randomURLSafe(32) }

// NewNonce returns a random OIDC nonce, echoed back inside the id_token and
// checked on the callback so a stolen authorization code cannot be replayed
// against a session that never asked for it.
func NewNonce() (string, error) { return randomURLSafe(32) }

// NewCodeVerifier returns a PKCE code_verifier per RFC 7636: 43-128 characters
// from the unreserved URL set. 32 random bytes, base64url-encoded, is 43
// characters and satisfies both the length and character-set requirements in
// one step.
func NewCodeVerifier() (string, error) { return randomURLSafe(32) }

// CodeChallengeS256 derives the PKCE code_challenge (S256 method) from a
// verifier: base64url(sha256(verifier)), no padding.
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
