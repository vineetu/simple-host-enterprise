package auth

import (
	"strings"
	"testing"
	"time"
)

func testKey(id string, b byte) SigningKey {
	key := make([]byte, 32)
	for i := range key {
		key[i] = b
	}
	return SigningKey{ID: id, Key: key}
}

func TestSignAndVerifySessionRoundTrip(t *testing.T) {
	keys := []SigningKey{testKey("k1", 0x01)}
	value, err := SignSession(keys, "session-1", "user-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	verified, err := VerifySessionCookie(keys, value)
	if err != nil {
		t.Fatalf("VerifySessionCookie: %v", err)
	}
	if verified.SessionID != "session-1" || verified.UserID != "user-1" {
		t.Fatalf("verified = %+v", verified)
	}
}

func TestSignSessionRequiresAKey(t *testing.T) {
	if _, err := SignSession(nil, "s", "u", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected an error with no signing keys configured")
	}
}

func TestVerifySessionCookieRejectsExpired(t *testing.T) {
	keys := []SigningKey{testKey("k1", 0x01)}
	value, err := SignSession(keys, "session-1", "user-1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if _, err := VerifySessionCookie(keys, value); err == nil {
		t.Fatal("expected an expired cookie to be rejected")
	}
}

func TestVerifySessionCookieRejectsTamperedPayload(t *testing.T) {
	keys := []SigningKey{testKey("k1", 0x01)}
	value, err := SignSession(keys, "session-1", "user-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	parts := strings.SplitN(value, ".", 3)
	// Swap in a different (validly-shaped) payload without re-signing: the
	// signature must no longer match.
	forged, err := SignSession(keys, "session-EVIL", "user-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	forgedParts := strings.SplitN(forged, ".", 3)
	tampered := parts[0] + "." + forgedParts[1] + "." + parts[2]
	if _, err := VerifySessionCookie(keys, tampered); err == nil {
		t.Fatal("expected tampered payload to be rejected")
	}
}

func TestVerifySessionCookieRejectsUnknownKeyID(t *testing.T) {
	signingKeys := []SigningKey{testKey("k1", 0x01)}
	value, err := SignSession(signingKeys, "session-1", "user-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	verifyKeys := []SigningKey{testKey("k2", 0x02)} // no k1 at all
	if _, err := VerifySessionCookie(verifyKeys, value); err == nil {
		t.Fatal("expected an unknown key id to be rejected")
	}
}

func TestVerifySessionCookieRejectsMalformedValue(t *testing.T) {
	keys := []SigningKey{testKey("k1", 0x01)}
	for _, bad := range []string{"", "not-enough-parts", "a.b.c.d", "k1..sig", "unknown-id.payload.sig"} {
		if _, err := VerifySessionCookie(keys, bad); err == nil {
			t.Fatalf("VerifySessionCookie(%q) succeeded, want an error", bad)
		}
	}
}

// TestKeyRotationShape exercises the rotation sequence: add the
// new key second (both present, old still signs), swap the order (new
// signs, both still verify), remove the old key.
func TestKeyRotationShape(t *testing.T) {
	oldKey := testKey("old", 0x01)
	newKey := testKey("new", 0x02)

	// Step 1: only the old key. A cookie signed now must still verify once
	// the new key is added.
	valueFromOld, err := SignSession([]SigningKey{oldKey}, "s1", "u1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	// Step 2: both present, old still signs (old first).
	both := []SigningKey{oldKey, newKey}
	if _, err := VerifySessionCookie(both, valueFromOld); err != nil {
		t.Fatalf("cookie signed before rotation must still verify: %v", err)
	}
	valueFromOldStillSigning, err := SignSession(both, "s2", "u2", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if !strings.HasPrefix(valueFromOldStillSigning, "old.") {
		t.Fatalf("expected the first configured key (old) to sign, got %q", valueFromOldStillSigning)
	}

	// Step 3: swap order — new signs now, but both verify, including the
	// cookie minted under the old order.
	swapped := []SigningKey{newKey, oldKey}
	valueFromNew, err := SignSession(swapped, "s3", "u3", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if !strings.HasPrefix(valueFromNew, "new.") {
		t.Fatalf("expected the first configured key (new) to sign, got %q", valueFromNew)
	}
	if _, err := VerifySessionCookie(swapped, valueFromOldStillSigning); err != nil {
		t.Fatalf("cookie signed under the old order must still verify after swap: %v", err)
	}

	// Step 4: remove the old key — a cookie it signed is now rejected.
	onlyNew := []SigningKey{newKey}
	if _, err := VerifySessionCookie(onlyNew, valueFromOldStillSigning); err == nil {
		t.Fatal("expected a cookie signed by the removed key to be rejected")
	}
	if _, err := VerifySessionCookie(onlyNew, valueFromNew); err != nil {
		t.Fatalf("cookie signed by the retained key must still verify: %v", err)
	}
}
