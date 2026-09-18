package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksRefreshInterval is how long a fetched key set is trusted before a
// lookup that misses (an unknown kid) forces a refetch. Both Google and Dex
// rotate signing keys on the order of weeks, not minutes; a short TTL only
// buys freshness nobody needs at the cost of a discovery-server round trip on
// every few id_tokens.
const jwksRefreshInterval = 10 * time.Minute

// jwk is the subset of RFC 7517 this package understands: RSA public keys
// used for signature verification (the "sig" use). Google and Dex both sign
// with RS256, so that is the only algorithm this verifier speaks; an
// id_token signed with anything else is refused rather than silently
// accepted under a weaker check.
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// jwksCache fetches and caches a provider's signing keys, keyed by kid.
type jwksCache struct {
	client *http.Client
	uri    string

	mu       sync.Mutex
	fetched  time.Time
	byKid    map[string]*rsa.PublicKey
	lastErr  error
	fetching bool
}

func newJWKSCache(client *http.Client, uri string) *jwksCache {
	return &jwksCache{client: client, uri: uri, byKid: map[string]*rsa.PublicKey{}}
}

// key returns the RSA public key for kid, fetching (or refetching, once)
// when it is unknown. A single-flight-by-mutex is enough here: id_token
// verification is not a hot path shared across thousands of concurrent
// requests the way a CDN's edge would need, and correctness under a key
// rotation matters more than shaving a lock.
func (c *jwksCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	if key, ok := c.byKid[kid]; ok && time.Since(c.fetched) < jwksRefreshInterval {
		c.mu.Unlock()
		return key, nil
	}
	c.mu.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.byKid[kid]
	if !ok {
		return nil, fmt.Errorf("no signing key found for kid %q", kid)
	}
	return key, nil
}

func (c *jwksCache) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.uri, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: unexpected status %s", resp.Status)
	}
	var doc jwksDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	byKid := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k)
		if err != nil {
			continue // one malformed key in the set must not break the rest
		}
		byKid[k.Kid] = pub
	}
	if len(byKid) == 0 {
		return errors.New("JWKS contained no usable RSA signing keys")
	}

	c.mu.Lock()
	c.byKid = byKid
	c.fetched = time.Now()
	c.mu.Unlock()
	return nil
}

func rsaPublicKeyFromJWK(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}

// verifyRS256 checks an RS256 (PKCS#1 v1.5, SHA-256) signature over
// signingInput using the given public key.
func verifyRS256(pub *rsa.PublicKey, signingInput, signature []byte) error {
	sum := sha256.Sum256(signingInput)
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], signature)
}
