package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

// The popup chain: a page on the attacker's own host opens the base host's
// /auth/handoff with the attacker's own nonce hash, so the base host mints a
// code for the victim's session and sends the victim's browser to the
// attacker's host with it. The victim's browser holds no matching nonce, so
// its redemption fails — and that failed attempt must spend the code, or the
// attacker reads it off the popup's address bar and redeems it in their own
// browser, which does hold the nonce.
func TestHandoffCodeSpentByAnyFailedRedemption(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	victim, err := CreateOIDCUser(ctx, database, "victim", "sub-victim", "victim@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := CreateSession(ctx, database, victim.ID, time.Now().Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	attackerNonce := sha256.Sum256([]byte("attacker-nonce"))
	const host = "mallory.example.com"

	for _, first := range []struct {
		name  string
		host  string
		nonce []byte
	}{
		{"victim browser with no nonce cookie", host, nil},
		{"victim browser with its own stale nonce", host, func() []byte { s := sha256.Sum256([]byte("victim-nonce")); return s[:] }()},
		{"redeemed on the wrong host", "alice.example.com", attackerNonce[:]},
	} {
		t.Run(first.name, func(t *testing.T) {
			code := "code-" + randomHex(t, 8)
			if err := CreateHandoffCode(ctx, database, code, session.ID, host, attackerNonce[:]); err != nil {
				t.Fatal(err)
			}
			if _, err := RedeemHandoffCode(ctx, database, code, first.host, first.nonce); !errors.Is(err, ErrHandoffCodeInvalid) {
				t.Fatalf("first attempt: err = %v, want ErrHandoffCodeInvalid", err)
			}
			if _, err := RedeemHandoffCode(ctx, database, code, host, attackerNonce[:]); !errors.Is(err, ErrHandoffCodeInvalid) {
				t.Fatalf("attacker's redemption after a failed attempt: err = %v, want ErrHandoffCodeInvalid", err)
			}
		})
	}

	t.Run("a correct first attempt still succeeds once", func(t *testing.T) {
		code := "code-" + randomHex(t, 8)
		if err := CreateHandoffCode(ctx, database, code, session.ID, host, attackerNonce[:]); err != nil {
			t.Fatal(err)
		}
		got, err := RedeemHandoffCode(ctx, database, code, host, attackerNonce[:])
		if err != nil || got != session.ID {
			t.Fatalf("redeem = %q, %v", got, err)
		}
		if _, err := RedeemHandoffCode(ctx, database, code, host, attackerNonce[:]); !errors.Is(err, ErrHandoffCodeInvalid) {
			t.Fatalf("replay: err = %v", err)
		}
	})

	t.Run("the table never holds the code itself", func(t *testing.T) {
		code := "code-" + randomHex(t, 8)
		if err := CreateHandoffCode(ctx, database, code, session.ID, host, attackerNonce[:]); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := database.QueryRowContext(ctx, `SELECT count(*) FROM handoff_codes WHERE code = $1`, code).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("handoff_codes stores the plaintext code")
		}
	})
}

func TestPruneSessionsDeletesOnlyLongEnded(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	user, err := CreateOIDCUser(ctx, database, "pruned", "sub-pruned", "pruned@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	live, err := CreateSession(ctx, database, user.ID, time.Now().Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := CreateSession(ctx, database, user.ID, time.Now().Add(-100*24*time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateHandoffCode(ctx, database, "old-code", live.ID, "a.example.com", []byte("n")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE handoff_codes SET created_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := PruneSessions(ctx, database, 90)
	if err != nil || n != 1 {
		t.Fatalf("PruneSessions = %d, %v; want 1", n, err)
	}
	var remaining int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id IN ($1, $2)`, live.ID, old.ID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("sessions left = %d, %v; want only the live one", remaining, err)
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM handoff_codes`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("handoff codes left = %d, %v", remaining, err)
	}
}
