package handler

import (
	"strings"
	"testing"
)

func TestCheckGrantEmail(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		domains []string
		refused string
	}{
		{"plain address, no domain list", "new.person@corp.com", nil, ""},
		{"upper case and spaces are normalized", "  New.Person@Corp.com ", []string{"corp.com"}, ""},
		{"allowed domain", "a@corp.com", []string{"other.com", "corp.com"}, ""},
		{"other domain refused", "a@gmail.com", []string{"corp.com"}, "company email domain"},
		{"subdomain is not the domain", "a@eu.corp.com", []string{"corp.com"}, "company email domain"},
		{"no local part", "@corp.com", nil, "not an email"},
		{"no dot in domain", "a@corp", nil, "not an email"},
		{"two at signs", "a@b@corp.com", nil, "not an email"},
		{"space inside", "a b@corp.com", nil, "not an email"},
		{"leading dot in domain", "a@.corp.com", nil, "not an email"},
		{"trailing dot in domain", "a@corp.com.", nil, "not an email"},
		{"double dot in domain", "a@corp..com", nil, "not an email"},
		{"too long", strings.Repeat("a", 250) + "@corp.com", nil, "not an email"},
		{"plus and percent are fine", "a.b+tag%x_y-z@mail.corp.com", nil, ""},
		// Stored before its owner signs in and rendered back to other people:
		// anything that could close an attribute or open a tag is refused.
		{"double quote", "a\"onmouseover=x@corp.com", nil, "not an email"},
		{"single quote", "o'brien@corp.com", nil, "not an email"},
		{"angle brackets", "<b>@corp.com", nil, "not an email"},
		{"parentheses and equals", "a(x)=1@corp.com", nil, "not an email"},
		{"non-ASCII local part", "\u212aelvin@corp.com", nil, "not an email"},
		{"non-ASCII domain", "a@c\u00f6rp.com", nil, "not an email"},
		{"numeric top-level label", "a@corp.123", nil, "not an email"},
		{"label starting with hyphen", "a@-corp.com", nil, "not an email"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkGrantEmail(tc.email, tc.domains)
			if tc.refused == "" && got != "" {
				t.Fatalf("checkGrantEmail(%q) = %q, want accepted", tc.email, got)
			}
			if tc.refused != "" && !strings.Contains(got, tc.refused) {
				t.Fatalf("checkGrantEmail(%q) = %q, want a refusal containing %q", tc.email, got, tc.refused)
			}
		})
	}
}

// Usernames pass through untouched; only the emails in a batch are checked.
func TestCheckGrantEmailsChecksOnlyEmails(t *testing.T) {
	if got := checkGrantEmails([]string{"alice", "team-sales", "bob@corp.com"}, []string{"corp.com"}); got != "" {
		t.Fatalf("mixed batch refused: %q", got)
	}
	if got := checkGrantEmails([]string{"alice", "eve@elsewhere.com"}, []string{"corp.com"}); got == "" {
		t.Fatal("an email outside ALLOWED_EMAIL_DOMAINS was accepted")
	}
}

// TestPageEscapeCoversQuotes guards the other half of the viewer-email fix:
// every page script that builds rows with innerHTML escapes quotes too, so a
// stored string can never close the attribute it is placed in.
func TestPageEscapeCoversQuotes(t *testing.T) {
	for name, script := range map[string]string{
		"dashboard sites":   dashboardSitesScript,
		"dashboard deleted": dashboardDeletedScript,
		"admin activity":    adminActivityScript,
	} {
		i := strings.Index(script, "function esc(s)")
		if i < 0 {
			t.Fatalf("%s: no esc()", name)
		}
		body := script[i : i+strings.Index(script[i:], "\n")]
		if !strings.Contains(body, "&quot;") || !strings.Contains(body, "&#39;") {
			t.Fatalf("%s: esc() does not escape quotes: %s", name, body)
		}
	}
}
