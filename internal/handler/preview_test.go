package handler

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
)

// A preview token verifies for the site, version and expiry it was signed
// for, under any configured key, and for nothing else. The signature is
// base64url, which has '-' in it, so many are tried.
func TestPreviewTokenRoundTrip(t *testing.T) {
	old := auth.SigningKey{ID: "old", Key: []byte(strings.Repeat("o", 32))}
	current := auth.SigningKey{ID: "new", Key: []byte(strings.Repeat("n", 32))}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	for i := 0; i < 300; i++ {
		site := "site-" + strconv.Itoa(i)
		token := previewToken([]auth.SigningKey{current}, site, 3, expires)
		version, at, ok := parsePreviewToken([]auth.SigningKey{current, old}, site, token)
		if !ok || version != 3 || !at.Equal(expires) {
			t.Fatalf("token %q for %s: version %d at %v ok %v", token, site, version, at, ok)
		}
		if _, _, ok := parsePreviewToken([]auth.SigningKey{current}, site+"x", token); ok {
			t.Fatalf("token for %s verified for another site", site)
		}
		if _, _, ok := parsePreviewToken([]auth.SigningKey{old}, site, token); ok {
			t.Fatalf("token verified under a key it was not signed with")
		}
		parts := strings.SplitN(token, "-", 3)
		if _, _, ok := parsePreviewToken([]auth.SigningKey{current}, site, "4-"+parts[1]+"-"+parts[2]); ok {
			t.Fatalf("token verified for another version")
		}
	}
	for _, bad := range []string{"", "3", "3-1", "x-1-abc", "0-1-abc", "3-x-abc", "3-1-***"} {
		if _, _, ok := parsePreviewToken([]auth.SigningKey{current}, "s", bad); ok {
			t.Fatalf("%q verified", bad)
		}
	}
}
