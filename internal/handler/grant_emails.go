package handler

import (
	"strings"
	"unicode"

	"github.com/vsriram/simple-host/internal/db"
)

// Site viewers and team members may be named by company email as well as by
// username: someone who has not signed in yet gets a pending grant that
// becomes the real one at their first sign-in (db.ConvertPendingGrants).

// checkGrantEmail validates one email given where a username is accepted:
// a minimal shape check, then the same domain rule sign-in applies
// (ALLOWED_EMAIL_DOMAINS, when set). It returns the refusal to show, or "".
func checkGrantEmail(email string, allowedDomains []string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || len(email) > 254 || strings.Contains(domain, "@") ||
		!strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") ||
		strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") ||
		strings.IndexFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' }) >= 0 {
		return "\"" + email + "\" is not an email address"
	}
	if !(OIDCClaimConfig{AllowedEmailDomains: allowedDomains}).isAllowedDomain(email) {
		return "\"" + email + "\" is not at a company email domain that can sign in here"
	}
	return ""
}

// checkGrantEmails applies checkGrantEmail to every email in a batch of
// names, returning the first refusal, or "".
func checkGrantEmails(names []string, allowedDomains []string) string {
	for _, name := range names {
		if db.IsEmailName(name) {
			if refusal := checkGrantEmail(name, allowedDomains); refusal != "" {
				return refusal
			}
		}
	}
	return ""
}
