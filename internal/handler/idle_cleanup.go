package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
)

// IdleCleanup is the opt-in cleanup of sites nobody uses
// (IDLE_CLEANUP_DAYS): a site nobody has opened (owner and team included),
// deployed to, or read or written saved data on for that many days
// (db.siteLastUsed) is marked; the dashboard tells its owner (or team)
// and, with SMTP set, so does an email; 30 days later (db.IdleGrace), still
// unused and not kept, it moves to Recently deleted, where it can be
// restored for another 30 days. Any use or Keep unmarks it. Every step is
// audited. It runs on every replica; each step is atomic per site, so two
// replicas never mark, notify or delete the same site twice.
type IdleCleanup struct {
	database *sql.DB
	audit    audit.Recorder
	idleFor  time.Duration
	mailer   Mailer
	base     string
}

// Mailer sends one plain-text message. SMTPMailer is the real one.
type Mailer interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// NewIdleCleanup returns the cleanup for days of disuse; mailer may be nil
// (dashboard notice only).
func NewIdleCleanup(database *sql.DB, recorder audit.Recorder, days int, mailer Mailer, publicBaseURL string) *IdleCleanup {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &IdleCleanup{database: database, audit: recorder, idleFor: time.Duration(days) * 24 * time.Hour, mailer: mailer, base: strings.TrimRight(publicBaseURL, "/")}
}

// Run runs RunOnce a minute after start and then hourly until ctx ends.
func (c *IdleCleanup) Run(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("idle cleanup: %v", err)
		}
		timer.Reset(time.Hour)
	}
}

// RunOnce unmarks sites used since they were marked, marks newly idle ones
// (and tells their owners), and moves the ones whose grace has run out to
// Recently deleted.
func (c *IdleCleanup) RunOnce(ctx context.Context) error {
	if err := c.step(ctx, "site_idle_cleared", func(tx *sql.Tx) ([]db.IdleSite, error) { return db.ClearUsedIdleSites(ctx, tx) }, nil); err != nil {
		return fmt.Errorf("unmark used sites: %w", err)
	}
	notified := "dashboard"
	if c.mailer != nil {
		notified = "dashboard and email"
	}
	marked := []db.IdleSite{}
	if err := c.step(ctx, "site_idle_marked", func(tx *sql.Tx) ([]db.IdleSite, error) {
		sites, err := db.MarkIdleSites(ctx, tx, c.idleFor)
		marked = sites
		return sites, err
	}, map[string]any{"notified": notified}); err != nil {
		return fmt.Errorf("mark idle sites: %w", err)
	}
	for _, s := range marked {
		c.notify(ctx, s)
	}
	due, err := db.DueIdleSites(ctx, c.database)
	if err != nil {
		return fmt.Errorf("list idle sites due: %w", err)
	}
	for _, s := range due {
		if err := c.moveToRecentlyDeleted(ctx, s); err != nil {
			log.Printf("idle cleanup: delete %s/%s: %v", s.Owner, s.Name, err)
		}
	}
	return nil
}

// step runs one set-based change and records one audit event per site it
// touched, in the same transaction.
func (c *IdleCleanup) step(ctx context.Context, action string, change func(*sql.Tx) ([]db.IdleSite, error), extra map[string]any) error {
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	sites, err := change(tx)
	if err != nil {
		return err
	}
	for _, s := range sites {
		detail := map[string]any{"site": s.Name, "last_used": s.LastUsed.UTC().Format(time.RFC3339)}
		if action == "site_idle_marked" {
			detail["moves_to_recently_deleted_on"] = s.DeleteOn().UTC().Format("2006-01-02")
		}
		for k, v := range extra {
			detail[k] = v
		}
		if err := c.audit.RecordTx(ctx, tx, audit.Event{ActorKind: "system", Action: action, OwnerID: s.OwnerID, SiteID: s.ID, Extra: detail}); err != nil {
			return err
		}
	}
	return audit.Commit(tx)
}

// moveToRecentlyDeleted deletes one due site the way its owner would, under
// its lock and only if it is still due, audited as site_delete with reason
// "idle".
func (c *IdleCleanup) moveToRecentlyDeleted(ctx context.Context, s db.IdleSite) error {
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(ctx, tx, s.OwnerID, s.Name); err != nil {
		return err
	}
	// The row lock orders this check against a visit, a saved-data write or
	// a Keep (each updates the row): one that committed first is seen here,
	// one that comes later waits and then finds the site gone.
	if err := db.LockSiteRow(ctx, tx, s.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if due, err := db.StillDueIdle(ctx, tx, s); err != nil || !due {
		return err
	}
	if err := db.EnqueueSiteSearch(ctx, tx, s.ID, db.SiteSearchDelete); err != nil {
		return err
	}
	if err := db.SoftDeleteSite(ctx, tx, s.ID, s.OwnerID, s.Name, ""); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := db.ClearIdleMark(ctx, tx, s.ID); err != nil {
		return err
	}
	if err := c.audit.RecordTx(ctx, tx, audit.Event{
		ActorKind: "system", Action: "site_delete", OwnerID: s.OwnerID, SiteID: s.ID,
		Extra: map[string]any{
			"reason": "idle", "active_version": s.ActiveVersion, "last_used": s.LastUsed.UTC().Format(time.RFC3339),
			"restorable_until": time.Now().Add(db.DeletedSiteRetention).UTC().Format(time.RFC3339),
		},
	}); err != nil {
		return err
	}
	return audit.Commit(tx)
}

// notify emails the owner (or every member of the team) that a site was
// marked. Best effort: a failure is logged, and the dashboard notice stands.
func (c *IdleCleanup) notify(ctx context.Context, s db.IdleSite) {
	if c.mailer == nil {
		return
	}
	to, err := db.IdleNoticeRecipients(ctx, c.database, s.OwnerID)
	if err != nil || len(to) == 0 {
		if err != nil {
			log.Printf("idle cleanup: recipients for %s/%s: %v", s.Owner, s.Name, err)
		}
		return
	}
	subject := fmt.Sprintf("%s/%s moves to Recently deleted on %s", s.Owner, s.Name, s.DeleteOn().UTC().Format("2 January 2006"))
	body := fmt.Sprintf("The site %s/%s has not been opened or changed in %d days (last used %s).\r\n\r\n"+
		"On %s it will move to Recently deleted, where it can still be restored for 30 days.\r\n\r\n"+
		"To keep it, open %s/dashboard and choose Keep, or download a copy there. Opening or updating the site also keeps it.\r\n",
		s.Owner, s.Name, idleDays(s.LastUsed, time.Now()), s.LastUsed.UTC().Format("2 January 2006"), s.DeleteOn().UTC().Format("2 January 2006"), c.base)
	if err := c.mailer.Send(ctx, to, subject, body); err != nil {
		log.Printf("idle cleanup: email about %s/%s: %v", s.Owner, s.Name, err)
	}
}

// keepSite answers POST /api/sites/{sitename}/keep and
// /api/collaboration/sites/{owner}/{sitename}/keep: the owner or a team
// member says the site stays ({"keep": true}, the default) or may be
// cleaned up again ({"keep": false}). Audited as site_idle_keep.
func (h *SiteHandler) keepSite(w http.ResponseWriter, r *http.Request) {
	user, access, ok := h.movableSite(w, r)
	if !ok {
		return
	}
	req := struct {
		Keep *bool `json:"keep"`
	}{}
	if r.ContentLength != 0 && !decodeSmallJSON(w, r, &req) {
		return
	}
	keep := req.Keep == nil || *req.Keep
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	// Under the site's lock, and only if the caller still manages the same
	// site: a Keep never lands on a site the cleanup (or its owner) deleted,
	// or one moved somewhere the caller cannot manage, in the meantime.
	if err := db.LockSiteCollaboration(r.Context(), tx, access.OwnerID, access.Site.Name); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	locked, err := db.ResolveSiteAccess(r.Context(), tx, access.ActorID, access.OwnerUsername, access.Site.Name)
	if err != nil || locked.Site.ID != access.Site.ID || !grantsOwnerRole(locked.Role) {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("keep %s/%s: recheck access: %v", access.OwnerUsername, access.Site.Name, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}
	was, err := db.SetSiteIdleKeep(r.Context(), tx, access.Site.ID, keep)
	if err == nil {
		actorKind, keyID := auditActorKind(r.Context())
		extra := map[string]any{"keep": keep}
		if was != nil {
			extra["was_marked_idle_since"] = was.UTC().Format(time.RFC3339)
		}
		err = h.audit.RecordTx(r.Context(), tx, audit.Event{
			ActorID: user.ID, ActorKind: actorKind, KeyID: keyID, Action: "site_idle_keep",
			OwnerID: access.OwnerID, SiteID: access.Site.ID, RequestID: auditRequestID(r.Context()), Extra: extra,
		})
	}
	if err == nil {
		err = audit.Commit(tx)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}
	if err != nil {
		log.Printf("keep %s/%s: %v", access.OwnerUsername, access.Site.Name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"owner": access.OwnerUsername, "site": access.Site.Name, "keep": keep})
}

// idleDays is how many whole days a site has gone unused.
func idleDays(lastUsed, now time.Time) int {
	return int(now.Sub(lastUsed) / (24 * time.Hour))
}

// idleNoticeHTML is the dashboard notice for the person's marked sites, one
// row each with the date and Keep / Download; "" when there are none.
func idleNoticeHTML(ctx context.Context, database *sql.DB, user *db.User) string {
	sites, err := db.ListIdleSites(ctx, database, user.ID)
	if err != nil {
		log.Printf("dashboard: idle sites for %s: %v", user.Username, err)
		return ""
	}
	if len(sites) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<section id="idle-section">
  <h2 class="section-title">Not used lately</h2>
  <p class="login-copy">These sites have not been opened or changed in a long while. Each moves to Recently deleted on the date shown (and can be restored for 30 days after that). Keep it, download a copy, or just open it.</p>
  <div class="rank-list" role="region" aria-label="Sites not used lately">`)
	for _, s := range sites {
		fmt.Fprintf(&b, `<div class="rank-row idle-row"><span class="rank-name">%s/%s <span class="rank-sub">Not opened or changed in %d days · moves to Recently deleted on %s</span></span>
  <button type="button" class="btn-login idle-keep" data-owner="%s" data-site="%s">Keep</button> <button type="button" class="btn-reject idle-download" data-owner="%s" data-site="%s">Download</button></div>`,
			html.EscapeString(s.Owner), html.EscapeString(s.Name),
			idleDays(s.LastUsed, time.Now()), localTimeHTML(s.DeleteOn(), "date"),
			html.EscapeString(s.Owner), html.EscapeString(s.Name), html.EscapeString(s.Owner), html.EscapeString(s.Name))
	}
	b.WriteString(`</div>
</section>
<script>
// Download is the whole-site zip (files, saved data and its history,
// versions, uploaded files) through a single-use 10-minute link
// (site_export.go).
document.querySelectorAll('.idle-download').forEach(function(button){
  button.addEventListener('click', function(){
    button.disabled = true;
    var path = '/api/collaboration/sites/' + encodeURIComponent(button.getAttribute('data-owner')) + '/' + encodeURIComponent(button.getAttribute('data-site')) + '/export-link';
    fetch(path, {method: 'POST', credentials: 'same-origin', headers: {'X-Simple-Host-Client': 'control-ui'}})
      .then(function(r){ return r.json().then(function(b){
        button.disabled = false;
        if (!r.ok || !b.url) throw new Error();
        location.href = b.url;
      }); })
      .catch(function(){ button.disabled = false; button.textContent = 'Try again'; });
  });
});
document.querySelectorAll('.idle-keep').forEach(function(button){
  button.addEventListener('click', function(){
    button.disabled = true;
    var path = '/api/collaboration/sites/' + encodeURIComponent(button.getAttribute('data-owner')) + '/' + encodeURIComponent(button.getAttribute('data-site')) + '/keep';
    fetch(path, {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json', 'X-Simple-Host-Client': 'control-ui'}, body: '{"keep":true}'})
      .then(function(r){
        if (!r.ok) throw new Error();
        var row = button.closest('.idle-row');
        row.querySelector('.rank-sub').textContent = 'Kept';
        button.remove();
      })
      .catch(function(){ button.disabled = false; button.textContent = 'Try again'; });
  });
});
</script>`)
	return b.String()
}

// renderIdleSites is the /admin card of every marked site, soonest to go
// first. Left out when there are none.
func (h *AdminHandler) renderIdleSites(r *http.Request, b *strings.Builder) {
	sites, err := db.ListIdleSites(r.Context(), h.database, "")
	if err != nil {
		log.Printf("admin: idle sites: %v", err)
		return
	}
	if len(sites) == 0 {
		return
	}
	fmt.Fprintf(b, `<section class="overview" id="admin-idle"><div class="overview-card"><h2 class="section-title">Not used lately <span class="card-count">%d</span></h2>
<p class="login-copy">Marked by the idle cleanup: each moves to Recently deleted on the date shown unless its owner or team keeps it or it is used again.</p><div class="rank-list" role="region" aria-label="Sites not used lately">`, len(sites))
	for _, s := range sites {
		fmt.Fprintf(b, `<div class="rank-row"><span class="rank-name">%s/%s <span class="rank-sub">last used %s</span></span><span class="rank-metric">moves %s</span></div>`,
			html.EscapeString(s.Owner), html.EscapeString(s.Name), localTimeHTML(s.LastUsed, "date"), localTimeHTML(s.DeleteOn(), "date"))
	}
	b.WriteString(`</div></div></section>`)
}
