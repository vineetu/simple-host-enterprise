package identityhash

import (
	"bytes"
	"strings"
	"testing"
)

func TestKeyedHashesAndSignatures(t *testing.T) {
	t.Cleanup(func() { _ = Configure(nil) })
	_ = Configure(nil)
	legacy := Subject("https://idp/", "sub")
	if strings.Contains(legacy, ":") || !Valid(legacy) {
		t.Fatalf("unconfigured hash = %q, want a plain SHA-256", legacy)
	}
	if err := Configure([]Key{{ID: "new", Key: bytes.Repeat([]byte{1}, 32)}, {ID: "old", Key: bytes.Repeat([]byte{2}, 32)}}); err != nil {
		t.Fatal(err)
	}
	keyed := Subject("https://idp", "sub")
	if !strings.HasPrefix(keyed, "new:") || !Valid(keyed) || keyed == legacy {
		t.Fatalf("keyed hash = %q", keyed)
	}
	if Email("A@B.example") != Email(" a@b.example ") || Email("a@b.example") == Subject("", "a@b.example") {
		t.Fatal("email hash not normalised, or not separated from subject hashes")
	}
	candidates := SubjectCandidates("https://idp", "sub")
	if len(candidates) != 3 || candidates[0] != keyed || !strings.HasPrefix(candidates[1], "old:") || candidates[2] != legacy {
		t.Fatalf("candidates = %v", candidates)
	}
	for _, bad := range []string{"", ":" + strings.Repeat("a", 64), strings.Repeat("A", 64), "k:" + strings.Repeat("a", 63)} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
	body := []byte(`{"site":"x"}`)
	sig := SignManifest(body)
	if ok, known := VerifyManifest(body, sig); !ok || !known {
		t.Fatalf("own signature: ok=%v known=%v", ok, known)
	}
	if ok, _ := VerifyManifest([]byte(`{"site":"y"}`), sig); ok {
		t.Fatal("a changed body verified")
	}
	if _, known := VerifyManifest(body, "gone:"+strings.Repeat("0", 64)); known {
		t.Fatal("an unconfigured key id was known")
	}
}
