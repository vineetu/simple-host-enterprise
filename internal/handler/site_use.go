package handler

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

// A site is "used" when anyone opens it (its owner and team included, bots
// and previews not), reads or writes its saved data, or deploys to it. The
// idle cleanup judges a site by that (sites.last_used_at); a site's use is
// written at most once per db.SiteUseThrottle per replica, off the request
// path, and the database skips a write within the same window.

func newSiteUseMarker() *stateUsageMarker {
	return newStateUsageMarkerWith(4, defaultStateUsageMaxCompleted, db.SiteUseThrottle, time.Now)
}

// markSiteOpened records a successful open of one of owner's site's pages
// or files, unless a bot made it: an uptime check or a crawler is not use.
func markSiteOpened(marker *stateUsageMarker, database *sql.DB, r *http.Request, status int, owner, site string) {
	if status < 200 || status >= 400 || classifyClient(r).isBot() {
		return
	}
	markSiteUsed(marker, database, owner, site)
}

// markSiteUsed records one use of owner's site: a saved-data read or write
// (by a browser or a script alike) or an open markSiteOpened let through.
func markSiteUsed(marker *stateUsageMarker, database *sql.DB, owner, site string) {
	if marker == nil || database == nil {
		return
	}
	marker.Schedule(owner+"/"+site, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), analyticsWriteLimit)
		defer cancel()
		err := db.MarkSiteUsed(ctx, database, owner, site)
		if err != nil {
			log.Printf("mark %s/%s used: %v", owner, site, err)
		}
		return err
	})
}
