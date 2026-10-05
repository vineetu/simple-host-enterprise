package handler

import "net/http"

// Keep selected homes on the site's existing origin. The person address opens
// the home only after checking its ordinary access rules; all content and data
// retain the site's existing host-bound session.
func (g *hostGate) servePersonHome(w http.ResponseWriter, r *http.Request, label, requestHost string) bool {
	if g.personHome == nil || !g.hosts.OwnerReady(label) {
		return false
	}
	userID, _, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return true
	}
	owner, ok := g.resolveLabelHolder(label)
	if !ok {
		return false
	}
	name, err := g.personHome(r.Context(), owner)
	if err != nil {
		http.Error(w, "failed to load home page", 500)
		return true
	}
	if name == "" {
		return false
	}
	siteID, _, err := g.siteForServing(r, owner, name)
	if err != nil {
		return false
	}
	// A home never changes visibility. A caller without access sees their normal index.
	allowed, err := g.viewerAllowed(r, siteID, userID)
	if err != nil {
		http.Error(w, "failed to load home page", 500)
		return true
	}
	if !allowed {
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		methodNotAllowed(w, "GET, HEAD")
		return true
	}
	target := g.hosts.SiteURL(owner, name)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
	return true
}
