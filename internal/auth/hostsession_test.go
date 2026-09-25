package auth

import (
	"testing"
	"time"
)

var hostSessionTestKeys = []SigningKey{{ID: "k1", Key: []byte("0123456789abcdef0123456789abcdef")}}

// TestVerifyHostedSessionBindsToItsHost is the review-requested case: a
// cookie minted for one owner host must not verify on another. Without the
// Host claim (see docs/security-review.md's deviation record), a cookie
// captured or mis-delivered from alice's host would authenticate a
// request on bob's host too, since the session row itself is shared on
// purpose.
func TestVerifyHostedSessionBindsToItsHost(t *testing.T) {
	cookie, err := SignHostSession(hostSessionTestKeys, "session-1", "user-1", "alice.example.com", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignHostSession: %v", err)
	}

	if uid, sid, ok := VerifyHostedSession(hostSessionTestKeys, nil, cookie, "alice.example.com"); !ok || uid != "user-1" || sid != "session-1" {
		t.Fatalf("VerifyHostedSession(own host) = %q, %q, %t; want user-1, session-1, true", uid, sid, ok)
	}
	// Case-insensitive: HostModel.Classify normalizes to lowercase, but the
	// check should not depend on the caller having already done so.
	if _, _, ok := VerifyHostedSession(hostSessionTestKeys, nil, cookie, "ALICE.EXAMPLE.COM"); !ok {
		t.Fatal("VerifyHostedSession should match case-insensitively")
	}

	if _, _, ok := VerifyHostedSession(hostSessionTestKeys, nil, cookie, "bob.example.com"); ok {
		t.Fatal("a cookie minted for alice's host verified on bob's host")
	}
}

// TestVerifyHostedSessionRefusesAnUnboundCookie covers the base-host
// cookie (SignSession, no Host claim) and any other cookie minted without
// SignHostSession: hosted content must refuse it outright, not treat a
// missing Host as "any host."
func TestVerifyHostedSessionRefusesAnUnboundCookie(t *testing.T) {
	cookie, err := SignSession(hostSessionTestKeys, "session-1", "user-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if _, _, ok := VerifyHostedSession(hostSessionTestKeys, nil, cookie, "alice.example.com"); ok {
		t.Fatal("an unbound (base-host) cookie verified as a hosted-content session")
	}
}

func TestSignHostSessionRejectsAnEmptyHost(t *testing.T) {
	if _, err := SignHostSession(hostSessionTestKeys, "session-1", "user-1", "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("SignHostSession accepted an empty host")
	}
}
