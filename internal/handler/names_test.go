package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestValidateOwnerName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"dotted name", "alice.smith", nil},
		{"plain name", "alice", nil},
		{"digits and dashes", "a1-b2", nil},
		{"reserved www", "www", errOwnerNameReserved},
		{"reserved api", "api", errOwnerNameReserved},
		{"reserved via lowercasing", "WWW", errOwnerNameReserved},
		{"reserved via dot mapping", "simple.host", errOwnerNameReserved},
		{"punycode prefix", "xn--alice", errOwnerNameNotLabel},
		{"leading dash", "-alice", errOwnerNameNotLabel},
		{"trailing dash", "alice-", errOwnerNameNotLabel},
		{"leading dot becomes leading dash", ".alice", errOwnerNameNotLabel},
		{"too long for a label", strings.Repeat("a", 64), errOwnerNameNotLabel},
		{"uppercase is not its own label", "Alice", errOwnerNameNotLabel},
		{"underscore is not a label character", "a_b", errOwnerNameNotLabel},
		{"empty", "", errOwnerNameUnsafe},
		{"dot", ".", errOwnerNameUnsafe},
		{"dot dot", "..", errOwnerNameUnsafe},
		{"separator", "a/b", errOwnerNameUnsafe},
		{"over the segment limit", strings.Repeat("a", 256), errOwnerNameUnsafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateOwnerName(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("validateOwnerName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateTypedName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"plain name", "acme-ai", nil},
		{"digits and dashes", "a1-b2", nil},
		{"single character", "a", nil},
		{"longest allowed label", strings.Repeat("a", 63), nil},

		{"dotted name", "acme.ai", errTypedNameDotted},
		{"dotted name that folds onto a hyphen spelling", "alice.smith", errTypedNameDotted},
		{"trailing dot", "acme.", errTypedNameDotted},

		{"uppercase", "Acme", errOwnerNameNotLabel},
		{"reserved www", "www", errOwnerNameReserved},
		{"newly reserved setup", "setup", errOwnerNameReserved},
		{"newly reserved skills", "skills", errOwnerNameReserved},
		{"newly reserved fonts", "fonts", errOwnerNameReserved},
		// The dot is reported even though the hyphen spelling is reserved
		// too: the dot is what was typed, and it is refused on its own.
		{"reserved and dotted", "simple.host", errTypedNameDotted},

		{"leading hyphen", "-acme", errOwnerNameNotLabel},
		{"trailing hyphen", "acme-", errOwnerNameNotLabel},
		{"over 63 characters", strings.Repeat("a", 64), errOwnerNameNotLabel},
		{"punycode prefix", "xn--acme", errOwnerNameNotLabel},
		{"underscore", "acme_ai", errOwnerNameNotLabel},
		{"empty", "", errOwnerNameUnsafe},
		{"separator", "a/b", errOwnerNameUnsafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateTypedName(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("validateTypedName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDottedNameStillPassesValidateOwnerName(t *testing.T) {
	// The typed-name rule must not leak into the derived path: registration
	// keeps folding a dotted email local part into a hyphenated label, so
	// these names have to stay registrable.
	for _, name := range []string{"acme.ai", "alice.smith", "a.b.c"} {
		if err := validateOwnerName(name); err != nil {
			t.Errorf("validateOwnerName(%q) = %v, want nil", name, err)
		}
		if err := validateTypedName(name); !errors.Is(err, errTypedNameDotted) {
			t.Errorf("validateTypedName(%q) = %v, want errTypedNameDotted", name, err)
		}
	}
}

func TestReservedLabelsAreThemselvesValidLabels(t *testing.T) {
	// A reserved entry that isValidLabel would reject is dead weight, and one
	// with uppercase or dots could never match ownerLabel's output.
	for label := range reservedLabels {
		if !isValidLabel(label) {
			t.Errorf("reservedLabels entry %q is not a valid label and can never match", label)
		}
	}
	for name := range reservedSiteNames {
		if !isValidLabel(name) {
			t.Errorf("reservedSiteNames entry %q is not a valid label and can never match", name)
		}
	}
}

func TestIsOwnerLabelViolation(t *testing.T) {
	labelClash := &pq.Error{Code: "23505", Constraint: ownerLabelIndexName}
	if !isOwnerLabelViolation(labelClash) {
		t.Fatal("label index violation not recognised")
	}
	usernameClash := &pq.Error{Code: "23505", Constraint: "users_username_key"}
	if isOwnerLabelViolation(usernameClash) {
		t.Fatal("username uniqueness violation misread as a label clash")
	}
	if !isUniqueViolation(usernameClash) || !isUniqueViolation(labelClash) {
		t.Fatal("both violations must still count as unique violations")
	}
	if isOwnerLabelViolation(&pq.Error{Code: "23503", Constraint: ownerLabelIndexName}) {
		t.Fatal("non-unique error code accepted")
	}
	if isOwnerLabelViolation(errors.New("plain")) {
		t.Fatal("plain error accepted")
	}
}

// TestOIDCUsernameDerivationRefusesReservedAndUnaddressableNames covers the
// name check the OIDC sign-in path (auth.go's createUser) applies before
// ever reaching the database: the same rule the old email-registration
// endpoint applied, now checked directly rather than through a deleted
// HTTP route.
func TestOIDCUsernameDerivationRefusesReservedAndUnaddressableNames(t *testing.T) {
	cases := []struct {
		email    string
		reserved bool
	}{
		{"www@example.com", true},
		{"Admin@example.com", true},
		{"simple.host@example.com", true},
		{"-alice@example.com", false},
		{"alice-@example.com", false},
	}
	for _, tc := range cases {
		derived := deriveUsernameFromEmail(tc.email)
		err := validateOwnerName(derived)
		if err == nil {
			t.Errorf("validateOwnerName(deriveUsernameFromEmail(%q)) = nil, want an error", tc.email)
			continue
		}
		if errors.Is(err, errOwnerNameReserved) != tc.reserved {
			t.Errorf("validateOwnerName(deriveUsernameFromEmail(%q)) = %v, want reserved=%t", tc.email, err, tc.reserved)
		}
	}
	// An email whose local part is too long to be a DNS label must fail
	// deriveUsernameFromEmail itself (empty result) or validateOwnerName;
	// either way it must never reach account creation.
	long := strings.Repeat("a", 64) + "@example.com"
	if derived := deriveUsernameFromEmail(long); derived != "" && validateOwnerName(derived) == nil {
		t.Errorf("email with a too-long local part produced a usable username %q", derived)
	}
}

func TestCreateSiteRefusesReservedSiteNames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sites/{sitename}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := validatedNewSiteName(w, r); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	for _, name := range []string{"sites", "api"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sites/"+name, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("POST /api/sites/%s status = %d, want 400", name, response.Code)
		}
		if !strings.Contains(response.Body.String(), "site name is reserved") {
			t.Errorf("POST /api/sites/%s body = %q, want reserved message", name, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sites/my-site", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("POST /api/sites/my-site status = %d, want 204", response.Code)
	}
}

func TestExistingSiteWithReservedNameStillPassesGeneralValidation(t *testing.T) {
	// Updates, rollbacks and deletes go through validatedSiteName, which must
	// keep accepting a reserved name so a site created before the rule can
	// still be managed.
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/sites/{sitename}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := validatedSiteName(w, r); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	for _, name := range []string{"sites", "api"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/sites/"+name, nil))
		if response.Code != http.StatusNoContent {
			t.Errorf("PUT /api/sites/%s status = %d, want 204", name, response.Code)
		}
	}
}
