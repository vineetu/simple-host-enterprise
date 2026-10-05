package db

import (
	"context"
	"database/sql"
	"testing"
)

func TestPersonHomeLifecycle(t *testing.T) {
	d := assetsTestDB(t)
	ctx := context.Background()
	alice, site := mustCreateUserAndSite(t, d, "alice", "home")
	bob, _ := mustCreateUserAndSite(t, d, "bob", "other")
	if _, err := d.Exec(`UPDATE sites SET active_version=0 WHERE id=$1::uuid`, site); err != nil {
		t.Fatal(err)
	}
	name := "other"
	if err := SetPersonHome(ctx, d, alice, &name); err != sql.ErrNoRows {
		t.Fatalf("foreign home: %v", err)
	}
	name = "home"
	if err := SetPersonHome(ctx, d, alice, &name); err != nil {
		t.Fatal(err)
	}
	if got, err := ServingPersonHome(ctx, d, "alice"); err != nil || got != "" {
		t.Fatalf("unpublished home: %q %v", got, err)
	}
	if _, err := d.Exec(`UPDATE sites SET name='renamed',active_version=1 WHERE id=$1::uuid`, site); err != nil {
		t.Fatal(err)
	}
	if got, err := ServingPersonHome(ctx, d, "alice"); err != nil || got != "renamed" {
		t.Fatalf("rename: %q %v", got, err)
	}
	if _, err := d.Exec(`UPDATE sites SET access_decision='restricted' WHERE id=$1::uuid`, site); err != nil {
		t.Fatal(err)
	}
	if got, err := ServingPersonHome(ctx, d, "alice"); err != nil || got != "" {
		t.Fatalf("restricted home: %q %v", got, err)
	}
	if _, err := d.Exec(`UPDATE sites SET access_decision=NULL,deleted_at=now() WHERE id=$1::uuid`, site); err != nil {
		t.Fatal(err)
	}
	pref, err := GetPersonPresentation(ctx, d, alice)
	if err != nil || pref.Home != nil {
		t.Fatalf("delete: %+v %v", pref, err)
	}
	if _, err := d.Exec(`UPDATE sites SET deleted_at=NULL WHERE id=$1::uuid`, site); err != nil {
		t.Fatal(err)
	}
	name = "renamed"
	if err := SetPersonHome(ctx, d, alice, &name); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE sites SET user_id=$2::uuid WHERE id=$1::uuid`, site, bob); err != nil {
		t.Fatal(err)
	}
	pref, err = GetPersonPresentation(ctx, d, alice)
	if err != nil || pref.Home != nil {
		t.Fatalf("transfer: %+v %v", pref, err)
	}
}
func TestPersonShowcaseSettings(t *testing.T) {
	d := assetsTestDB(t)
	ctx := context.Background()
	alice, _ := mustCreateUserAndSite(t, d, "alice", "home")
	if err := SetPersonBio(ctx, d, alice, "A short bio"); err != nil {
		t.Fatal(err)
	}
	pin := true
	order := 10
	if _, err := SetShowcasePreference(ctx, d, alice, "home", &pin, &order); err != nil {
		t.Fatal(err)
	}
	prefs, err := GetShowcasePreferences(ctx, d, alice)
	if err != nil || !prefs["home"].Pinned || prefs["home"].Order != 10 {
		t.Fatalf("preferences: %v %v", prefs, err)
	}
	pref, err := GetPersonPresentation(ctx, d, alice)
	if err != nil || pref.Bio != "A short bio" {
		t.Fatalf("bio: %v %v", pref, err)
	}
	if _, err := SetShowcasePreference(ctx, d, alice, "foreign", &pin, nil); err != sql.ErrNoRows {
		t.Fatalf("foreign pin: %v", err)
	}
}
