package db

import (
	"context"
	"errors"
	"testing"
)

func TestLastAdminCannotBeDisabledAndAdminEmailsSync(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	alice, err := CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := CreateOIDCUser(ctx, database, "bob", "sub-bob", "bob@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetUserDisabled(ctx, database, bob.ID, true); err != nil {
		t.Fatalf("disable one of two admins: %v", err)
	}
	if err := SetUserDisabled(ctx, database, alice.ID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable the last enabled admin: err = %v, want ErrLastAdmin", err)
	}

	// Removing bob from ADMIN_EMAILS demotes him at startup; alice stays.
	if _, err := SyncAdminEmails(ctx, database, []string{"alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	var aliceAdmin, bobAdmin bool
	database.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = $1`, alice.ID).Scan(&aliceAdmin)
	database.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = $1`, bob.ID).Scan(&bobAdmin)
	if !aliceAdmin || bobAdmin {
		t.Fatalf("after sync: alice admin = %v, bob admin = %v", aliceAdmin, bobAdmin)
	}
	if n, err := SyncAdminEmails(ctx, database, []string{"alice@example.com"}); err != nil || n != 0 {
		t.Fatalf("second sync changed %d rows (%v), want 0", n, err)
	}
}
