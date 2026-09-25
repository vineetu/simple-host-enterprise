package handler

import (
	"testing"

	"github.com/vsriram/simple-host/internal/oidc"
)

// sanitizeRedirectPath is shared by the post-sign-in redirect (auth.go) and
// the hand-off's final redirect (handoff.go's redeemHandoffSession) — one
// validator for both "to" parameters. This table is the adversarial half:
// every shape a login-CSRF or open-redirect attempt
// against either caller would try, plus the ordinary accepted and rejected
// shapes the rule states in plain words.
func TestSanitizeRedirectPath(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "ordinary path", in: "/my-site/", want: "/my-site/"},
		{name: "path with query", in: "/dashboard?notice=welcome", want: "/dashboard?notice=welcome"},
		{name: "bare slash", in: "/", want: "/"},

		{name: "no leading slash", in: "evil.com", want: ""},
		{name: "protocol-relative", in: "//evil.com", want: ""},
		{name: "scheme", in: "https://evil.com", want: ""},
		{name: "scheme, no slashes", in: "javascript:alert(1)", want: ""},
		{name: "userinfo", in: "https://user@evil.com/", want: ""},
		{name: "backslash instead of slash", in: "/\\evil.com", want: ""},
		{name: "leading backslash-slash", in: "/\\/evil.com", want: ""},
		{name: "backslash later in the path", in: "/ok/\\evil.com", want: ""},

		// A raw control character makes url.Parse itself refuse to parse the
		// value ("invalid control character in URL"); this function treats
		// that the same as any other rejection, and never inspects these
		// bytes itself. Each of these hides "//evil.com" behind a byte a
		// browser, a proxy, or a header-parsing layer might strip before the
		// value reaches here — the point of the case is that rejection does
		// not depend on any of that being true, only on url.Parse's own
		// refusal to parse the byte at all.
		{name: "tab hides the double slash", in: "/\t//evil.com", want: ""},
		{name: "newline hides the double slash", in: "/\n//evil.com", want: ""},
		{name: "carriage return hides the double slash", in: "/\r//evil.com", want: ""},

		// Percent-encoded slashes are accepted unchanged: see the comment on
		// sanitizeRedirectPath for why this cannot become a network-path
		// reference (a leading "/%" can never decode into a leading "//" at
		// the position a URL parser — Go's or a browser's — inspects to
		// decide there is an authority to resolve).
		{name: "percent-encoded double slash", in: "/%2F%2Fevil.com", want: "/%2F%2Fevil.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeRedirectPath(test.in); got != test.want {
				t.Errorf("sanitizeRedirectPath(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestRefuseIdentity(t *testing.T) {
	verified := func(extra map[string]any) oidc.Claims {
		return oidc.Claims{EmailVerified: true, EmailVerifiedSet: true, Raw: extra}
	}
	for _, test := range []struct {
		name       string
		cfg        OIDCClaimConfig
		claims     oidc.Claims
		email      string
		wantReason string
	}{
		{"verified", OIDCClaimConfig{Issuer: "https://dex.example"}, verified(nil), "a@corp.com", ""},
		{"email_verified absent", OIDCClaimConfig{Issuer: "https://dex.example"}, oidc.Claims{}, "a@corp.com", "email_not_verified"},
		{"email_verified false", OIDCClaimConfig{Issuer: "https://dex.example"}, oidc.Claims{EmailVerifiedSet: true}, "a@corp.com", "email_not_verified"},
		{"entra with xms_edov", OIDCClaimConfig{Issuer: "https://login.microsoftonline.com/tid/v2.0"}, oidc.Claims{Raw: map[string]any{"xms_edov": true}}, "a@corp.com", ""},
		{"entra without xms_edov", OIDCClaimConfig{Issuer: "https://login.microsoftonline.com/tid/v2.0"}, oidc.Claims{Raw: map[string]any{}}, "a@corp.com", "email_not_verified"},
		{"xms_edov from a non-Entra issuer", OIDCClaimConfig{Issuer: "https://dex.example"}, oidc.Claims{Raw: map[string]any{"xms_edov": true}}, "a@corp.com", "email_not_verified"},
		{"domain not allowed", OIDCClaimConfig{AllowedEmailDomains: []string{"corp.com"}}, verified(nil), "a@evil.com", "domain_not_allowed"},
		{"google consumer account at the company domain", OIDCClaimConfig{Issuer: "https://accounts.google.com", AllowedEmailDomains: []string{"corp.com"}}, verified(nil), "a@corp.com", "google_hd_missing"},
		{"google workspace account", OIDCClaimConfig{Issuer: "https://accounts.google.com", AllowedEmailDomains: []string{"corp.com"}}, oidc.Claims{EmailVerified: true, EmailVerifiedSet: true, HostedDomain: "corp.com"}, "a@corp.com", ""},
		{"google other workspace", OIDCClaimConfig{Issuer: "https://accounts.google.com", AllowedEmailDomains: []string{"corp.com"}}, oidc.Claims{EmailVerified: true, EmailVerifiedSet: true, HostedDomain: "other.com"}, "a@corp.com", "google_hd_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason, message := test.cfg.refuseIdentity(test.claims, test.email)
			if reason != test.wantReason || (reason == "") != (message == "") {
				t.Fatalf("refuseIdentity = %q, %q; want reason %q", reason, message, test.wantReason)
			}
		})
	}
}
