package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func pendingEmails(t *testing.T, database *sql.DB, table, idColumn, id string) []string {
	t.Helper()
	rows, err := database.Query(`SELECT email FROM `+table+` WHERE `+idColumn+` = $1 ORDER BY email`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			t.Fatal(err)
		}
		emails = append(emails, email)
	}
	return emails
}

// A viewer named by an email nobody has signed in with is kept pending,
// listed as such, removable by email, and turned into a real viewer at
// that person's first sign-in.
func TestPendingSiteViewers(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	mustCreateUserAndSite(t, database, "vera", "v")

	var viewers []SiteViewer
	if err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		viewers, err = GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"New.Person@Example.com", "vera@example.com"})
		return err
	}); err != nil {
		t.Fatalf("GrantSiteViewers: %v", err)
	}
	if len(viewers) != 2 || viewers[0].Username != "vera" || viewers[0].Pending ||
		viewers[1].Username != "new.person@example.com" || !viewers[1].Pending || viewers[1].Kind != "person" {
		t.Fatalf("viewers = %+v, want vera then pending new.person@example.com", viewers)
	}
	var access string
	if err := database.QueryRow(`SELECT access FROM sites WHERE id = $1`, siteID).Scan(&access); err != nil || access != AccessSpecific {
		t.Fatalf("access = %q (%v), want specific", access, err)
	}

	// Removable by email; a second removal finds nothing.
	for i, want := range []bool{true, false} {
		var removed, pending bool
		if err := inTx(t, database, func(tx *sql.Tx) error {
			var err error
			removed, pending, err = RevokeSiteViewer(ctx, tx, ownerID, "demo", siteID, "new.person@example.com")
			return err
		}); err != nil {
			t.Fatalf("RevokeSiteViewer: %v", err)
		}
		if removed != want || pending != want {
			t.Fatalf("revoke %d: removed=%v pending=%v, want %v", i, removed, pending, want)
		}
	}
	// A real viewer is removable by the email on their account too.
	if err := inTx(t, database, func(tx *sql.Tx) error {
		removed, pending, err := RevokeSiteViewer(ctx, tx, ownerID, "demo", siteID, "vera@example.com")
		if err == nil && (!removed || pending) {
			err = fmt.Errorf("removed=%v pending=%v, want a real viewer removed", removed, pending)
		}
		return err
	}); err != nil {
		t.Fatalf("revoke vera by email: %v", err)
	}

	// Re-add pending, then sign the person in.
	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"new.person@example.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	user, err := CreateOIDCUser(ctx, database, "new.person", "sub-new", "new.person@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	var converted []ConvertedGrant
	if err := inTx(t, database, func(tx *sql.Tx) error {
		converted, err = ConvertPendingGrants(ctx, tx, user.ID, "New.Person@example.com")
		return err
	}); err != nil {
		t.Fatalf("ConvertPendingGrants: %v", err)
	}
	if len(converted) != 1 || converted[0].SiteID != siteID || converted[0].OwnerID != ownerID {
		t.Fatalf("converted = %+v, want the one site", converted)
	}
	if left := pendingEmails(t, database, "pending_site_viewers", "site_id", siteID); len(left) != 0 {
		t.Fatalf("pending rows left after conversion: %v", left)
	}
	if ok, err := ViewerAllowed(ctx, database, siteID, user.ID); err != nil || !ok {
		t.Fatalf("ViewerAllowed after conversion = %v (%v), want true", ok, err)
	}
	viewers, err = ListSiteViewers(ctx, database, siteID)
	if err != nil || len(viewers) != 1 || viewers[0].Username != "new.person" || viewers[0].Pending {
		t.Fatalf("viewers after conversion = %+v (%v)", viewers, err)
	}
}

// Pending viewers count toward the 50-viewer cap, and die with their site.
func TestPendingSiteViewersCapAndCascade(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	emails := make([]string, MaxSiteViewers)
	for i := range emails {
		emails[i] = fmt.Sprintf("p%02d@example.com", i)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, emails)
		return err
	}); err != nil {
		t.Fatalf("grant %d pending: %v", len(emails), err)
	}
	// Re-adding one already pending is not a new viewer.
	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, emails[:1])
		return err
	}); err != nil {
		t.Fatalf("re-grant pending: %v", err)
	}
	err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"one.more@example.com"})
		return err
	})
	if !errors.Is(err, ErrViewerLimit) {
		t.Fatalf("51st viewer error = %v, want ErrViewerLimit", err)
	}

	if _, err := database.Exec(`DELETE FROM sites WHERE id = $1`, siteID); err != nil {
		t.Fatal(err)
	}
	if left := pendingEmails(t, database, "pending_site_viewers", "site_id", siteID); len(left) != 0 {
		t.Fatalf("pending viewers outlived their site: %v", left)
	}
}

// A team member named by an email nobody has signed in with is pending,
// counts toward the cap, is removable, joins at first sign-in, and dies with
// the team.
func TestPendingTeamMembers(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	creatorID, _ := mustCreateUserAndSite(t, database, "mo", "m")
	mustCreateUserAndSite(t, database, "bob", "b")

	var team User
	if err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		team, err = CreateTeam(ctx, tx, "crew", creatorID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var pending []string
	if err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		pending, err = AddTeamMembers(ctx, tx, team.ID, []string{"bob@example.com", "Later@Example.com"}, creatorID)
		return err
	}); err != nil {
		t.Fatalf("AddTeamMembers: %v", err)
	}
	if len(pending) != 1 || pending[0] != "later@example.com" {
		t.Fatalf("pending = %v, want [later@example.com]", pending)
	}
	members, err := ListTeamMembers(ctx, database, team.ID)
	if err != nil || len(members) != 3 || members[2].Username != "later@example.com" || !members[2].Pending || members[0].Pending {
		t.Fatalf("members = %+v (%v), want bob, mo, then pending later@example.com", members, err)
	}

	// Pending counts toward the cap: 3 so far, 47 more pending fill it.
	fill := make([]string, MaxTeamMembers-3)
	for i := range fill {
		fill[i] = fmt.Sprintf("f%02d@example.com", i)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := AddTeamMembers(ctx, tx, team.ID, fill, creatorID)
		return err
	}); err != nil {
		t.Fatalf("fill to the cap: %v", err)
	}
	err = inTx(t, database, func(tx *sql.Tx) error {
		_, err := AddTeamMembers(ctx, tx, team.ID, []string{"over@example.com"}, creatorID)
		return err
	})
	if !errors.Is(err, ErrTeamMemberLimit) {
		t.Fatalf("member past the cap error = %v, want ErrTeamMemberLimit", err)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error {
		removed, err := RemovePendingTeamMember(ctx, tx, team.ID, fill[0])
		if err == nil && !removed {
			err = errors.New("pending member not removed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	user, err := CreateOIDCUser(ctx, database, "later", "sub-later", "later@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	var converted []ConvertedGrant
	if err := inTx(t, database, func(tx *sql.Tx) error {
		converted, err = ConvertPendingGrants(ctx, tx, user.ID, "later@example.com")
		return err
	}); err != nil {
		t.Fatalf("ConvertPendingGrants: %v", err)
	}
	if len(converted) != 1 || converted[0].TeamID != team.ID {
		t.Fatalf("converted = %+v, want the team", converted)
	}
	if ok, err := IsTeamMember(ctx, database, team.ID, user.ID); err != nil || !ok {
		t.Fatalf("IsTeamMember after conversion = %v (%v)", ok, err)
	}

	if _, err := database.Exec(`DELETE FROM users WHERE id = $1`, team.ID); err != nil {
		t.Fatal(err)
	}
	if left := pendingEmails(t, database, "pending_team_members", "team_id", team.ID); len(left) != 0 {
		t.Fatalf("pending members outlived their team: %v", left)
	}
}
