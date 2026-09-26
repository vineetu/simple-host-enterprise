package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// discoveryDocument is the subset of RFC 8414 / OpenID Connect Discovery this
// package needs. Every field is required at the provider's issuer: Google and
// Dex both publish all four, and a provider missing any of them cannot serve
// Authorization Code + PKCE, which is the only flow this package speaks.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover fetches "<issuer>/.well-known/openid-configuration" and validates
// that the document's own issuer matches what was configured — the check
// OpenID Connect Discovery 1.0 section 4.3 requires, and the one line that
// stops a compromised or misconfigured proxy in front of the issuer from
// substituting a different provider's endpoints.
func discover(ctx context.Context, client *http.Client, issuer string, insecureAllowed bool) (discoveryDocument, error) {
	issuer = strings.TrimRight(issuer, "/")
	if err := requireHTTPS("issuer", issuer, insecureAllowed); err != nil {
		return discoveryDocument{}, err
	}
	url := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return discoveryDocument{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("fetch discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discoveryDocument{}, fmt.Errorf("fetch discovery document: unexpected status %s", resp.Status)
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return discoveryDocument{}, fmt.Errorf("decode discovery document: %w", err)
	}
	if strings.TrimRight(doc.Issuer, "/") != issuer {
		return discoveryDocument{}, fmt.Errorf("discovery document issuer %q does not match configured issuer %q", doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return discoveryDocument{}, errors.New("discovery document is missing authorization_endpoint, token_endpoint, or jwks_uri")
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"jwks_uri":               doc.JWKSURI,
	} {
		if err := requireHTTPS(name, endpoint, insecureAllowed); err != nil {
			return discoveryDocument{}, err
		}
	}
	return doc, nil
}

// requireHTTPS refuses a plain-http issuer or endpoint: the client secret
// travels in the token request and the JWKS decides whose ID tokens are
// believed, so neither may cross the network unauthenticated.
// OIDC_INSECURE_ALLOWED=true (the local overlay's in-cluster Dex) permits it.
func requireHTTPS(name, raw string, insecureAllowed bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s is not an absolute URL", name)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && insecureAllowed:
		return nil
	}
	return fmt.Errorf("%s must use https (OIDC_INSECURE_ALLOWED=true permits http on a local evaluation cluster only)", name)
}
