package handler

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// InstanceStatus is what the /admin "This instance" card shows: the running
// release, whether the schema has migrations waiting, this replica's latest
// bucket check, owner-certificate readiness, and the limits in force. main
// fills it from the build, the configuration and the metrics registry.
type InstanceStatus struct {
	Version, Commit, Schema string
	// PendingMigrations names the migrations this binary embeds that the
	// database has not applied. Optional.
	PendingMigrations func(context.Context) ([]string, error)
	// Bucket is this replica's latest /readyz bucket check. Optional.
	Bucket func() (ok, known bool, at time.Time)
	// OwnerCertsManual is OWNER_CERTS=manual: the operator provides every
	// owner's certificate, so there is nothing to wait for.
	OwnerCertsManual bool
	// Limits are the effective settings, in display order.
	Limits []InstanceLimit
}

// InstanceLimit is one effective setting: a plain name and its value.
type InstanceLimit struct {
	Name, Value string
}

// WithInstanceStatus attaches what the "This instance" card shows. Without
// it the card is left out.
func (h *AdminHandler) WithInstanceStatus(status InstanceStatus) *AdminHandler {
	h.status = &status
	return h
}

// renderInstanceStatus writes the "This instance" card. Every lookup is
// best effort: a failure shows as "could not check" rather than failing
// the page.
func (h *AdminHandler) renderInstanceStatus(r *http.Request, b *strings.Builder) {
	s := h.status
	if s == nil {
		return
	}
	now := time.Now()
	row := func(name, value string) {
		fmt.Fprintf(b, `<div class="rank-row"><span class="rank-name">%s</span><span class="rank-metric">%s</span></div>`, html.EscapeString(name), value)
	}
	b.WriteString(`<section class="overview" id="admin-instance"><div class="overview-card"><h2 class="section-title">This instance</h2><div class="rank-list" role="region" aria-label="Instance status">`)
	row("Release", html.EscapeString(fmt.Sprintf("%s · commit %s · schema %s", s.Version, shortCommit(s.Commit), s.Schema)))

	if s.PendingMigrations != nil {
		pending, err := s.PendingMigrations(r.Context())
		switch {
		case err != nil:
			log.Printf("admin: pending migrations: %v", err)
			row("Migrations", "could not check")
		case len(pending) == 0:
			row("Migrations", "schema is current")
		default:
			row("Migrations", html.EscapeString(fmt.Sprintf("%d waiting: %s (run simple-host migrate)", len(pending), strings.Join(pending, ", "))))
		}
	}

	if s.Bucket != nil {
		ok, known, at := s.Bucket()
		switch {
		case !known:
			row("Bucket", "not checked yet by this replica")
		case ok:
			row("Bucket", "reachable · checked "+localTimeHTML(at, "datetime"))
		default:
			row("Bucket", `<span class="status-bad">failing</span> · checked `+localTimeHTML(at, "datetime")+` · pages not in this replica's cache and publishing fail until it is fixed`)
		}
	}

	if s.OwnerCertsManual {
		row("Owner certificates", "provided by the operator (OWNER_CERTS=manual)")
	} else {
		ready, waiting, err := db.OwnerHostReadiness(r.Context(), h.database)
		if err != nil {
			log.Printf("admin: owner host readiness: %v", err)
			row("Owner certificates", "could not check")
		} else {
			row("Owner certificates", html.EscapeString(fmt.Sprintf("%d ready · %d waiting", ready, len(waiting))))
			for _, w := range waiting {
				row("  waiting: "+w.Label, html.EscapeString("for "+waitText(now.Sub(w.Since))+"; sites served at "+w.Label+".<base>/<site>/ meanwhile"))
			}
		}
	}

	for _, l := range s.Limits {
		row(l.Name, html.EscapeString(l.Value))
	}
	b.WriteString(`</div></div></section>`)
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// waitText is a wait as a person says it: minutes, hours, then days.
func waitText(d time.Duration) string {
	switch {
	case d < time.Hour:
		return pluralize(int(d/time.Minute), "1 minute", fmt.Sprintf("%d minutes", int(d/time.Minute)))
	case d < 48*time.Hour:
		return pluralize(int(d/time.Hour), "1 hour", fmt.Sprintf("%d hours", int(d/time.Hour)))
	default:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	}
}
