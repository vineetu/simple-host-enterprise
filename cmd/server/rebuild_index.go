package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/storage"
)

// runRebuildIndex lists every site the bucket alone can bring back after the
// database is lost (and its point-in-time recovery with it), from each
// site's manifest (internal/storage/manifest.go): owner, name, the versions
// and uploaded files that are there. With -apply it recreates each one whose
// owner is back in the database under the same site id, so every object's
// key still matches: pages, all kept versions and uploaded files, deleted
// sites back in Recently deleted and restricted ones restricted.
//
// A person's sites go to the account with the same sign-in identity (the
// manifest keeps a hash of issuer and subject), never to whoever holds the
// username now; a team's to the team with the same id. Anything else waits
// for the operator to map it (-map <manifest owner>=<account or team>),
// after checking who it was. Saved data, its history, access levels,
// viewers and team members were only ever in the database; a recreated site
// opens only for its owner or team until its access level is set again.
// Without -apply it changes nothing; -apply refuses a database that already
// has sites unless -force-live-db.
func runRebuildIndex(args []string) error {
	fs := flag.NewFlagSet("rebuild-index", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "recreate the sites listed as recoverable (default: list only)")
	forceLive := fs.Bool("force-live-db", false, "allow -apply against a database that already has sites (default: refused; a rebuild is for an empty database)")
	maps := ownerMap{}
	fs.Var(maps, "map", "`owner=account`: give the sites whose manifest names owner to that existing account or team, when no sign-in identity or team id matches; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	database, objects, err := openDatabaseAndObjects(cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	_, err = rebuildIndex(context.Background(), database, objects, os.Stdout, rebuildOptions{
		Apply: *apply, ForceLiveDB: *forceLive, Issuer: cfg.OIDC.Issuer, Map: maps,
	})
	return err
}

// ownerMap is -map's values: manifest owner name to account or team name.
type ownerMap map[string]string

func (m ownerMap) String() string { return fmt.Sprint(map[string]string(m)) }

func (m ownerMap) Set(value string) error {
	from, to, ok := strings.Cut(value, "=")
	if !ok || from == "" || to == "" {
		return errors.New("-map takes <manifest owner>=<account or team name>")
	}
	m[from] = to
	return nil
}

// rebuildOptions are one rebuild-index run's flags.
type rebuildOptions struct {
	Apply       bool
	ForceLiveDB bool
	// Issuer is OIDC_ISSUER, which the manifest's owner identity hashes.
	Issuer string
	Map    map[string]string
}

// rebuildResult counts what a rebuild-index run found and did.
type rebuildResult struct {
	Recoverable, Recreated, Waiting, Present, Unnamed, Refused int
}

// rebuildOwner is the account a site goes to, and how it was found.
type rebuildOwner struct {
	ID, Username, Kind, How string
}

// rebuildRun is what every site's decision reads.
type rebuildRun struct {
	database *sql.DB
	objects  storage.Objects
	recorder *audit.DBRecorder
	opts     rebuildOptions
	// byIdentity is every person account by its sign-in identity hash.
	byIdentity map[string][]rebuildOwner
	// ambiguous names manifest owners whose manifests disagree on who they
	// are (different identities or kinds under one name).
	ambiguous map[string]bool
	// manual collects the owners the operator must map or restore by hand.
	manual map[string]string
}

func rebuildIndex(ctx context.Context, database *sql.DB, objects storage.Objects, out io.Writer, opts rebuildOptions) (rebuildResult, error) {
	var res rebuildResult
	if opts.Apply && !opts.ForceLiveDB {
		var n int
		if err := database.QueryRowContext(ctx, `SELECT count(*) FROM sites`).Scan(&n); err != nil {
			return res, err
		}
		if n > 0 {
			return res, fmt.Errorf("the database already has %d sites: -apply rebuilds into the empty database of a lost install, where no site can have been deleted or purged since the bucket was written; run without -apply to list, or pass -force-live-db if this database is meant to be added to", n)
		}
	}
	sites, err := storage.ListRecoverableSites(ctx, objects)
	if err != nil {
		return res, fmt.Errorf("list the bucket: %w", err)
	}
	run := &rebuildRun{database: database, objects: objects, opts: opts,
		byIdentity: map[string][]rebuildOwner{}, ambiguous: map[string]bool{}, manual: map[string]string{}}
	people, err := db.ListPeopleWithSubjects(ctx, database)
	if err != nil {
		return res, err
	}
	for _, p := range people {
		hash := db.ErasedSubjectHash(opts.Issuer, p.Subject)
		run.byIdentity[hash] = append(run.byIdentity[hash], rebuildOwner{ID: p.ID, Username: p.Username, Kind: "person", How: "sign-in identity"})
	}
	who := map[string]string{}
	for _, s := range sites {
		if s.Manifest == nil {
			continue
		}
		m := s.Manifest
		key := m.OwnerKind + "|" + m.OwnerIdentity + "|" + m.TeamID
		if prev, seen := who[m.Owner]; seen && prev != key {
			run.ambiguous[m.Owner] = true
		}
		who[m.Owner] = key
	}
	for from := range opts.Map {
		if _, ok := who[from]; !ok {
			return res, fmt.Errorf("-map %s: no manifest names that owner", from)
		}
	}
	if opts.Apply {
		run.recorder = audit.NewDBRecorder(database)
		stream := audit.NewStream(slog.New(slog.NewJSONHandler(os.Stdout, nil)), 0)
		defer stream.Close(5 * time.Second)
		run.recorder.SetStream(stream)
	}

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE ID\tOWNER\tSITE\tVERSIONS\tLIVE\tFILES\tSTATUS")
	for _, s := range sites {
		owner, name, live := "?", "?", s.NewestVersion()
		if s.Manifest != nil {
			owner, name, live = s.Manifest.Owner, s.Manifest.Site, s.Manifest.LiveVersion
		}
		status := run.one(ctx, s, &res)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", s.SiteID, owner, name, len(s.Versions), live, len(s.AssetIDs), status)
	}
	if err := tw.Flush(); err != nil {
		return res, err
	}
	fmt.Fprintf(out, "\n%d sites in the bucket: %d recoverable now, %d recreated, %d waiting for their owner, %d refused, %d already in the database, %d without a manifest.\n",
		len(sites), res.Recoverable, res.Recreated, res.Waiting, res.Refused, res.Present, res.Unnamed)
	fmt.Fprintln(out, "Pages and uploaded files come back; saved data, its history, access levels, viewers and team members were only in the database and do not. A recreated site opens only for its owner or team until its access level is set again.")
	fmt.Fprintln(out, "Owners and names come from the bucket: anyone who could write to it could have changed a manifest. A person's sites go only to the account with the same sign-in identity, a team's only to the team with the same id; check the list before -apply.")
	if len(run.manual) > 0 {
		fmt.Fprintln(out, "\nNeeds you (check who each owner was before mapping; a team must be created again first):")
		names := make([]string, 0, len(run.manual))
		for name := range run.manual {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(out, "  %s: %s\n", name, run.manual[name])
		}
	}
	if !opts.Apply && res.Recoverable > 0 {
		fmt.Fprintln(out, "Nothing was changed. Run again with -apply to recreate the recoverable sites.")
	}
	return res, nil
}

// one decides one site's status and, with Apply, recreates it.
func (run *rebuildRun) one(ctx context.Context, s storage.RecoverableSite, res *rebuildResult) string {
	if s.Manifest == nil {
		res.Unnamed++
		why := "no manifest (not deployed since manifests were written)"
		if s.ManifestErr != nil {
			why = s.ManifestErr.Error()
		}
		if v := s.NewestVersion(); v > 0 {
			return why + fmt.Sprintf("; restore it by hand once you know its owner, name and live version: simple-host restore -from-site-id %s -version <n> -owner <owner> -site <name> (newest archive: v%d)", s.SiteID, v)
		}
		return why
	}
	m := s.Manifest
	refuse := func(why string) string {
		res.Refused++
		return "refused: " + why
	}
	if err := m.Validate(); err != nil {
		return refuse("the manifest is not valid: " + err.Error())
	}
	if err := handler.ValidateRebuiltNames(m.Owner, m.OwnerKind, m.Site); err != nil {
		return refuse(err.Error())
	}
	if !s.LiveArchive() {
		return refuse(fmt.Sprintf("the live version v%d's archive is not in the bucket, and no other version is made live without its owner; restore the one they approved by hand: simple-host restore -from-site-id %s -version <n> -owner <owner> -site %s", m.LiveVersion, s.SiteID, m.Site))
	}
	var purgeAt time.Time
	if m.DeletedAt != nil {
		purgeAt = m.DeletedAt.Add(db.DeletedSiteRetention())
		if !purgeAt.After(time.Now()) {
			return refuse("it was deleted on " + m.DeletedAt.Format("2006-01-02") + " and its recovery window has ended")
		}
	}
	var exists bool
	if err := run.database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sites WHERE id = $1::uuid)`, s.SiteID).Scan(&exists); err != nil {
		return "error: " + err.Error()
	}
	if exists {
		res.Present++
		return "already in the database"
	}
	owner, problem, err := run.resolveOwner(ctx, m)
	if err != nil {
		return "error: " + err.Error()
	}
	switch problem {
	case "":
	case "refused":
		res.Refused++
		return "refused: " + run.manual[m.Owner]
	default:
		res.Waiting++
		return "waiting: " + problem
	}
	state := ""
	if m.DeletedAt != nil {
		state += ", as deleted"
	}
	if m.Restricted {
		state += ", restricted"
	}
	if !run.opts.Apply {
		res.Recoverable++
		return fmt.Sprintf("recoverable: to %s (by %s)%s", owner.Username, owner.How, state)
	}
	if err := run.recreate(ctx, s, owner.ID, purgeAt); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			res.Refused++
			return "refused: " + owner.Username + " already has a site named " + m.Site
		}
		return "not recreated: " + err.Error()
	}
	res.Recreated++
	return fmt.Sprintf("recreated: to %s (by %s)%s", owner.Username, owner.How, state)
}

// resolveOwner finds the account a manifest's sites go to. problem is
// "" when found, "refused" when the manifests or the mapping contradict
// each other (run.manual says why), else why the site waits.
func (run *rebuildRun) resolveOwner(ctx context.Context, m *storage.SiteManifest) (owner rebuildOwner, problem string, err error) {
	if run.ambiguous[m.Owner] {
		run.manual[m.Owner] = "manifests name this owner with different sign-in identities or teams; restore each site by hand after checking whose it is"
		return owner, "refused", nil
	}
	var match *rebuildOwner
	switch m.OwnerKind {
	case storage.ManifestOwnerPerson:
		if m.OwnerIdentity != "" {
			found := run.byIdentity[m.OwnerIdentity]
			if len(found) > 1 {
				run.manual[m.Owner] = "more than one account has this sign-in identity"
				return owner, "refused", nil
			}
			if len(found) == 1 {
				match = &found[0]
			}
		}
	case storage.ManifestOwnerTeam:
		var t rebuildOwner
		err := run.database.QueryRowContext(ctx, `SELECT id::text, username FROM users WHERE id = $1::uuid AND kind = 'team'`, m.TeamID).Scan(&t.ID, &t.Username)
		if err == nil {
			t.Kind, t.How = "team", "team id"
			match = &t
		} else if !errors.Is(err, sql.ErrNoRows) {
			return owner, "", err
		}
	}
	mapped, hasMap := run.opts.Map[m.Owner]
	if match != nil {
		if hasMap && mapped != match.Username {
			run.manual[m.Owner] = fmt.Sprintf("-map says %s, but the %s belongs to %s", mapped, match.How, match.Username)
			return owner, "refused", nil
		}
		return *match, "", nil
	}
	if hasMap {
		var u rebuildOwner
		err := run.database.QueryRowContext(ctx, `SELECT id::text, username, kind FROM users WHERE username = $1`, mapped).Scan(&u.ID, &u.Username, &u.Kind)
		if errors.Is(err, sql.ErrNoRows) {
			return owner, "-map names " + mapped + ", which does not exist yet", nil
		}
		if err != nil {
			return owner, "", err
		}
		if u.Kind != m.OwnerKind {
			run.manual[m.Owner] = fmt.Sprintf("-map names %s, a %s, but the sites belonged to a %s", mapped, u.Kind, m.OwnerKind)
			return owner, "refused", nil
		}
		u.How = "-map"
		return u, "", nil
	}
	if m.OwnerKind == storage.ManifestOwnerTeam {
		run.manual[m.Owner] = "create the team again (a member does, from the dashboard), then run with -map " + m.Owner + "=<its name>"
		return owner, "the team " + m.Owner + " is not in the database; see below", nil
	}
	if m.OwnerIdentity == "" {
		run.manual[m.Owner] = "the account had no sign-in identity recorded; once its person has signed in, run with -map " + m.Owner + "=<their username>"
		return owner, m.Owner + " has no recorded sign-in identity; see below", nil
	}
	run.manual[m.Owner] = "waiting for this person to sign in once (matched by sign-in identity, not by name); or, if their identity changed, -map " + m.Owner + "=<their username>"
	return owner, m.Owner + " signs in once, then run again", nil
}

func (run *rebuildRun) recreate(ctx context.Context, s storage.RecoverableSite, ownerID string, purgeAt time.Time) error {
	m := s.Manifest
	var assets []db.SiteManifestAsset
	for _, a := range m.Assets {
		if s.AssetIDs[a.ID] {
			assets = append(assets, db.SiteManifestAsset{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, SHA256: a.SHA256})
		}
	}
	tx, err := run.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(ctx, tx, ownerID, m.Site); err != nil {
		return err
	}
	if err := db.InsertRebuiltSite(ctx, tx, db.RebuiltSite{
		SiteID: s.SiteID, OwnerID: ownerID, Name: m.Site, Versions: s.Versions, LiveVersion: m.LiveVersion, Assets: assets,
		DeletedAt: m.DeletedAt, PurgeAt: purgeAt, Restricted: m.Restricted, RestrictedReason: m.RestrictedReason,
		VersionKey: func(v int) string { key, _ := storage.VersionKey(s.SiteID, v); return key },
	}); err != nil {
		return err
	}
	if m.DeletedAt == nil {
		if err := db.EnqueueSiteSearch(ctx, tx, s.SiteID, db.SiteSearchReconcile); err != nil {
			return err
		}
	}
	if err := run.recorder.RecordTx(ctx, tx, audit.Event{
		ActorKind: "system", Action: "site_restore", OwnerID: ownerID, SiteID: s.SiteID,
		Extra: map[string]any{"from": "bucket_rebuild", "live_version": m.LiveVersion, "versions": len(s.Versions), "files": len(assets),
			"deleted": m.DeletedAt != nil, "restricted": m.Restricted},
	}); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	if err := audit.Commit(tx); err != nil {
		return err
	}
	if err := handler.WriteSiteManifest(ctx, run.database, run.objects, run.opts.Issuer, s.SiteID); err != nil {
		fmt.Fprintf(os.Stderr, "rebuild-index: rewrite the manifest of %s: %v\n", s.SiteID, err)
	}
	return nil
}
