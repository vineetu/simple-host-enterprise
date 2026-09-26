package db

import (
	"context"
	"reflect"
	"testing"
)

// OwnerLabelsWithSites lists each owner with at least one site once, by its
// host label (lowercase, '.' as '-'); the owner_hosts rows record and
// forget readiness per label.
func TestOwnerHosts(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	aliceID, _ := mustCreateUserAndSite(t, database, "Alice.Smith", "one")
	if _, err := CreateSite(ctx, database, aliceID, "two"); err != nil {
		t.Fatal(err)
	}
	mustCreateUserAndSite(t, database, "bob", "x")
	if _, err := CreateOIDCUser(ctx, database, "carol", "sub-carol", "carol@example.com", false); err != nil {
		t.Fatal(err)
	}

	owners, err := OwnerLabelsWithSites(ctx, database)
	if err != nil || !reflect.DeepEqual(owners, []string{"alice-smith", "bob"}) {
		t.Fatalf("OwnerLabelsWithSites = %v, %v", owners, err)
	}

	if ready, err := ReadyOwnerLabels(ctx, database); err != nil || len(ready) != 0 {
		t.Fatalf("ReadyOwnerLabels on an empty table = %v, %v", ready, err)
	}
	for _, step := range []struct {
		label string
		ready bool
	}{{"alice-smith", false}, {"bob", true}, {"alice-smith", true}, {"alice-smith", true}, {"bob", false}} {
		if err := SetOwnerHostReady(ctx, database, step.label, step.ready); err != nil {
			t.Fatalf("SetOwnerHostReady(%s, %t): %v", step.label, step.ready, err)
		}
	}
	if ready, err := ReadyOwnerLabels(ctx, database); err != nil || !reflect.DeepEqual(ready, []string{"alice-smith"}) {
		t.Fatalf("ReadyOwnerLabels = %v, %v", ready, err)
	}
	if err := SetOwnerHostReady(ctx, database, "dave", true); err != nil {
		t.Fatal(err)
	}
	if ready, _ := ReadyOwnerLabels(ctx, database); !reflect.DeepEqual(ready, []string{"alice-smith", "dave"}) {
		t.Fatalf("ReadyOwnerLabels = %v", ready)
	}

	if err := DeleteOwnerHostsExcept(ctx, database, []string{"alice-smith", "bob"}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := database.QueryRow(`SELECT count(*) FROM owner_hosts`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows after DeleteOwnerHostsExcept = %d, %v, want 2", rows, err)
	}
	// An empty keep list forgets everyone.
	if err := DeleteOwnerHostsExcept(ctx, database, []string{}); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT count(*) FROM owner_hosts`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after DeleteOwnerHostsExcept(empty) = %d, %v, want 0", rows, err)
	}
}
