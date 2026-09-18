package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testProvider spins up a fake discovery + JWKS + token endpoint, the way a
// real OIDC provider (Dex, Google) exposes them, and returns a *Provider
// wired to it plus the private key it signs with, so a test can mint its own
// id_tokens.
type testProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	// nextIDToken, when set, is returned verbatim from the token endpoint's
	// id_token field on the next Exchange call.
	nextIDToken string
}

func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tp := &testProvider{key: key, kid: "test-kid-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"jwks_uri": %q
		}`, tp.issuer(), tp.issuer()+"/auth", tp.issuer()+"/token", tp.issuer()+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(bigEndianExponent(key.PublicKey.E))
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`, tp.kid, n, e)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{IDToken: tp.nextIDToken})
	})
	tp.server = httptest.NewServer(mux)
	t.Cleanup(tp.server.Close)
	return tp
}

func (tp *testProvider) issuer() string { return tp.server.URL }

func bigEndianExponent(e int) []byte {
	// Standard exponent 65537 fits in 3 bytes; this is not a general-purpose
	// bigint encoder, just enough for what rsa.GenerateKey hands back.
	b := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

// mintIDToken signs a compact RS256 JWT with the test provider's key. claims
// is merged over the required registered claims, so a test can override
// iss/aud/exp/nonce to exercise a specific rejection.
func (tp *testProvider) mintIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "kid": tp.kid, "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, tp.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func baseClaims(issuer, clientID, nonce string) map[string]any {
	return map[string]any{
		"iss":            issuer,
		"aud":            clientID,
		"sub":            "user-123",
		"email":          "person@example.com",
		"email_verified": true,
		"exp":            float64(time.Now().Add(time.Hour).Unix()),
		"nonce":          nonce,
	}
}

func TestVerifyIDTokenAccepts(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback", Scopes: []string{"openid", "email"},
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	token := tp.mintIDToken(t, baseClaims(tp.issuer(), "client-1", "nonce-abc"))
	claims, err := p.VerifyIDToken(ctx, token, "nonce-abc")
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Subject != "user-123" || claims.Email != "person@example.com" || !claims.EmailVerified || !claims.EmailVerifiedSet {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestVerifyIDTokenRejectsWrongAudience(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	claims := baseClaims(tp.issuer(), "someone-elses-client", "nonce-abc")
	token := tp.mintIDToken(t, claims)
	if _, err := p.VerifyIDToken(ctx, token, "nonce-abc"); err == nil {
		t.Fatal("expected audience mismatch to be rejected")
	}
}

func TestVerifyIDTokenRejectsExpired(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	claims := baseClaims(tp.issuer(), "client-1", "nonce-abc")
	claims["exp"] = float64(time.Now().Add(-time.Hour).Unix())
	token := tp.mintIDToken(t, claims)
	if _, err := p.VerifyIDToken(ctx, token, "nonce-abc"); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestVerifyIDTokenRejectsWrongNonce(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	token := tp.mintIDToken(t, baseClaims(tp.issuer(), "client-1", "nonce-abc"))
	if _, err := p.VerifyIDToken(ctx, token, "nonce-different"); err == nil {
		t.Fatal("expected nonce mismatch to be rejected")
	}
}

func TestVerifyIDTokenRejectsBadSignature(t *testing.T) {
	tp := newTestProvider(t)
	other := newTestProvider(t) // different key, same issuer shape
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	claims := baseClaims(tp.issuer(), "client-1", "nonce-abc")
	forged := other.mintIDToken(t, claims) // signed by a different key entirely
	if _, err := p.VerifyIDToken(ctx, forged, "nonce-abc"); err == nil {
		t.Fatal("expected signature verification to fail for a token signed by a different key")
	}
}

func TestVerifyIDTokenRejectsUnknownKid(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	tp.kid = "rotated-kid" // JWKS now serves a different kid than the token below carries
	claims := baseClaims(tp.issuer(), "client-1", "nonce-abc")
	// Mint with the ORIGINAL kid recorded before rotation by re-creating the
	// token by hand with a stale kid header.
	header := map[string]any{"alg": "RS256", "kid": "stale-kid", "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, tp.key, crypto.SHA256, sum[:])
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := p.VerifyIDToken(ctx, token, "nonce-abc"); err == nil {
		t.Fatal("expected unknown kid to be rejected")
	}
}

func TestExchangeReturnsIDToken(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback",
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	tp.nextIDToken = "opaque-test-token"
	tok, err := p.Exchange(ctx, "some-code", "some-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.IDToken != "opaque-test-token" {
		t.Fatalf("IDToken = %q, want %q", tok.IDToken, "opaque-test-token")
	}
}

func TestPKCEChallengeIsDeterministic(t *testing.T) {
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier: %v", err)
	}
	if len(verifier) < 43 {
		t.Fatalf("verifier too short: %d chars", len(verifier))
	}
	c1 := CodeChallengeS256(verifier)
	c2 := CodeChallengeS256(verifier)
	if c1 != c2 {
		t.Fatal("CodeChallengeS256 must be deterministic for a given verifier")
	}
}

func TestAuthCodeURLCarriesPKCEAndHint(t *testing.T) {
	tp := newTestProvider(t)
	ctx := context.Background()
	p, err := Discover(ctx, Config{
		Issuer: tp.issuer(), ClientID: "client-1", ClientSecret: "secret",
		RedirectURL: "https://example.test/auth/callback", Scopes: []string{"openid", "email"},
	}, tp.server.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	req, err := NewAuthRequest()
	if err != nil {
		t.Fatalf("NewAuthRequest: %v", err)
	}
	authURL := p.AuthCodeURL(req, "example.com")
	if want := CodeChallengeS256(req.CodeVerifier); !strings.Contains(authURL, want) {
		t.Fatalf("auth URL missing code_challenge %q: %s", want, authURL)
	}
	if !strings.Contains(authURL, "hd=example.com") {
		t.Fatalf("auth URL missing hd hint: %s", authURL)
	}
}
