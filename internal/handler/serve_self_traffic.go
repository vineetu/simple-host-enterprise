package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	dbstore "github.com/vsriram/simple-host/internal/db"
)

// selfTrafficLookupLimit bounds the two lookups this check can make. It sits on
// the static-serving path, so a slow database must not hold a page open — on
// timeout we fail open and count the view.
const selfTrafficLookupLimit = 750 * time.Millisecond

// isSelfTraffic reports whether this request is the site's own owner or one of
// its editors browsing their own site, in which case analytics should skip it.
//
// Hosted sites currently share an origin with the account UI, so a signed-in
// user's session cookie is sent on /sites/... requests too. That is the only
// signal available here: static pages are public and carry no API key.
//
// Cost is deliberately shaped around the common case:
//
//   - No session cookie — almost every real visitor — costs nothing and counts.
//   - Cookie whose username matches the owner segment already in the URL costs
//     one lookup and skips, with no site query at all.
//   - Cookie belonging to somebody else costs one further query to see whether
//     they are an editor.
//
// Anything unidentifiable counts. An owner in a private window, on a phone, or
// signed out is indistinguishable from a stranger and will still be recorded;
// that is a known limit of doing this by cookie rather than by asking visitors
// who they are.
func isSelfTraffic(r *http.Request, database *sql.DB, signingKeys []auth.SigningKey, sessionIdle time.Duration, ownerUsername, siteName string) bool {
	if database == nil {
		return false
	}

	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	verified, err := auth.VerifySessionCookie(signingKeys, cookie.Value)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(r.Context(), selfTrafficLookupLimit)
	defer cancel()

	withUser, err := dbstore.GetValidSession(ctx, database, verified.SessionID, sessionIdle)
	if err != nil || withUser.Session.UserID != verified.UserID {
		// A stale, revoked, or forged cookie is not a signal about who is
		// reading; count it.
		if err != nil && !errors.Is(err, dbstore.ErrSessionInvalid) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("self-traffic check for %s/%s: resolve session: %v", ownerUsername, siteName, err)
		}
		return false
	}
	viewer := withUser.User

	// The owner's username is already the first path segment, so ownership needs
	// no query.
	if viewer.Username == ownerUsername {
		return true
	}

	_, err = dbstore.ResolveSiteAccess(ctx, database, viewer.ID, ownerUsername, siteName)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("self-traffic check for %s/%s: resolve access: %v", ownerUsername, siteName, err)
		}
		return false
	}
	return true
}
