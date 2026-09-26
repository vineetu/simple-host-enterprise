package migrate

import (
	"strings"
	"testing"
)

func TestSCRAMVerifierShape(t *testing.T) {
	v, err := scramSHA256Verifier("a-generated-hex-password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "SCRAM-SHA-256$4096:") || strings.Contains(v, "a-generated-hex-password") || strings.ContainsAny(v, "'\\") {
		t.Fatalf("verifier %q", v)
	}
	for _, bad := range []string{"", "pässword", "tab\there"} {
		if _, err := scramSHA256Verifier(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
