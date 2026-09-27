package handler

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/vsriram/simple-host/internal/db"
)

// Site viewers and team members may be named by company email as well as by
// username: someone who has not signed in yet gets a pending grant that
// becomes the real one at their first sign-in (db.ConvertPendingGrants).

// grantEmailPattern is the only shape a grant email may have: plain ASCII,
// a local part of letters, digits and ._%+-, and a domain of dot-separated
// labels ending in an alphabetic top-level label. Anything else (quotes,
// brackets, non-ASCII look-alikes) is refused, because the address is stored
// and shown back to other people before its owner ever signs in.
var grantEmailPattern = regexp.MustCompile(`^[a-z0-9._%+-]+@(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// checkGrantEmail validates one email given where a username is accepted:
// the strict shape above, then the same domain rule sign-in applies
// (ALLOWED_EMAIL_DOMAINS, when set). It returns the refusal to show, or "".
func checkGrantEmail(email string, allowedDomains []string) string {
	email = strings.TrimSpace(email)
	// ASCII before lower-casing: strings.ToLower folds some non-ASCII
	// letters (the Kelvin sign, for one) into ASCII ones.
	ascii := strings.IndexFunc(email, func(r rune) bool { return r > unicode.MaxASCII }) < 0
	email = strings.ToLower(email)
	if !ascii || len(email) > 254 || !grantEmailPattern.MatchString(email) {
		return strconv.Quote(email) + " is not an email address"
	}
	if !(OIDCClaimConfig{AllowedEmailDomains: allowedDomains}).isAllowedDomain(email) {
		return strconv.Quote(email) + " is not at a company email domain that can sign in here"
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
