package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagementValidatorRejectsRawEncodedControls(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/sites/{sitename}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := validatedSiteName(w, r); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	for _, target := range []string{"/api/sites/demo%0A", "/api/sites/demo%09", "/api/sites/%2e%2e"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s status = %d, want 400", target, response.Code)
		}
	}
}

func TestDeriveUsernameRejectsUnsafeFilesystemSegments(t *testing.T) {
	for _, email := range []string{".@example.com", "..@example.com", strings.Repeat("a", 256) + "@example.com"} {
		if got := deriveUsernameFromEmail(email); got != "" {
			t.Errorf("deriveUsernameFromEmail(%q) = %q, want empty", email, got)
		}
	}
	if got := deriveUsernameFromEmail("first.last+tag@example.com"); got != "first.lasttag" {
		t.Fatalf("deriveUsernameFromEmail valid result = %q", got)
	}
}

func TestSitePublicPathEscapesEachSegment(t *testing.T) {
	got := sitePublicPath("two words", "report#one?100%")
	want := "/sites/two%20words/report%23one%3F100%25/"
	if got != want {
		t.Fatalf("sitePublicPath = %q, want %q", got, want)
	}
}

func TestUserPublicPathAndListingActionEscapeSegments(t *testing.T) {
	if got, want := userPublicPath("two words#100%"), "/sites/two%20words%23100%25/"; got != want {
		t.Fatalf("userPublicPath = %q, want %q", got, want)
	}
	if got, want := userListingFormAction("alice&ops", "sign-in"), "/sites/alice&amp;ops/sign-in"; got != want {
		t.Fatalf("userListingFormAction = %q, want %q", got, want)
	}
}

// redirectWithTrailingSlash (the short-path redirect the host gate calls
// directly) is exercised by internal/handler/host_gate_test.go's
// TestHostGateRouting; the long-path redirect this test once covered
// (redirectToTrailingSlash, mux-registered on the base host) is gone with
// the rest of design.md 7.1's base-host site serving.
