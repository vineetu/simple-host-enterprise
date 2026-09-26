package handler

import (
	"database/sql"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// dashboardHeadHTML reuses the admin dashboard's own head and CSS block
// (it matches the existing admin/dashboard style), retitled. Sharing the
// constant, rather than a second copy of ~350 lines of
// CSS, is what keeps the two pages from drifting apart in appearance.
var dashboardHeadHTML = strings.Replace(adminHeadHTML, "Simple Host · Admin", "Simple Host · Dashboard", 1)

// DashboardHandler serves GET /dashboard: a sign-in prompt when signed out,
// and the signed-in person's API keys — mint, list, revoke — when signed in.
// The sessions page is a separate route, GET /auth/sessions (auth.go),
// linked from here.
type DashboardHandler struct {
	database    *sql.DB
	signingKeys []auth.SigningKey
	sessionIdle time.Duration
	quota       UploadQuota
}

// WithQuota sets the per-owner limits the page shows usage against.
func (h *DashboardHandler) WithQuota(quota UploadQuota) *DashboardHandler {
	h.quota = quota
	return h
}

func NewDashboardHandler(database *sql.DB, signingKeys []auth.SigningKey, sessionIdle time.Duration) *DashboardHandler {
	return &DashboardHandler{database: database, signingKeys: signingKeys, sessionIdle: sessionIdle}
}

func (h *DashboardHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	// Not behind authMiddleware: a signed-out visitor must see the sign-in
	// prompt, not a 401. The page reads the session itself via a best-effort
	// probe (auth.GetUser after a lightweight optional-auth wrapper) so it
	// can render either state without two routes.
	mux.HandleFunc("GET /dashboard", h.dashboard)
}

func (h *DashboardHandler) dashboard(w http.ResponseWriter, r *http.Request) {
	user := h.optionalUser(r)

	var b strings.Builder
	b.WriteString(dashboardHeadHTML)

	if user == nil {
		fmt.Fprintf(&b, `<header class="bar"><div class="mast">Simple Host<span class="dot">.</span> <span class="kicker">dashboard</span></div></header>
<main>
<section class="login-block">
  <h2 class="section-title">Sign in</h2>
  <p class="login-copy">Sign in with your work account to manage your API keys and sessions.</p>
  <p><a class="btn-login" href="/auth/login?to=%s">Sign in</a></p>
</section>
</main></body></html>`, html.EscapeString("/dashboard"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
		return
	}

	keys, err := db.ListAPIKeysForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("dashboard: list keys for %s: %v", user.Username, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	usageHTML := h.usageHTML(r, user)

	notice := ""
	if r.URL.Query().Get("notice") == "username_suffixed" {
		notice = `<section class="roadmap-block"><span class="roadmap-tag">Note</span>
  <span class="roadmap-text">Your usual username was already taken, so your account and site address use a suffixed version instead.</span>
</section>`
	}

	fmt.Fprintf(&b, `<header class="bar">
  <div class="mast" data-username="%s">Simple Host<span class="dot">.</span> <span class="kicker">dashboard</span></div>
  <nav class="dash-nav"><a href="/dashboard">Keys</a> <span class="sep" aria-hidden="true"></span> <a href="/auth/sessions">Sessions</a></nav>
  <form method="POST" action="/auth/logout" class="logout-form"><button type="submit" class="btn-logout">Sign out</button></form>
</header>
<main>%s
<h2 class="section-title">Signed in as %s%s</h2>

<section>
  <h2 class="section-title">API keys</h2>
  <p class="login-copy">A key authenticates CI or other automation as you. Mint one per job or machine so each can be revoked without touching the others. Keys expire; mint a fresh one when yours does. A publish key can deploy, update and roll back your sites and use their saved data and files; a full key can do everything you can, except administration.</p>
  <form id="mint-form" class="login-form" onsubmit="return false">
    <input type="text" id="key-name" placeholder="Name (e.g. laptop, CI)" maxlength="200" autocomplete="off">
    <select id="key-scope" aria-label="What the key can do">
      <option value="publish" selected>Publish</option>
      <option value="full">Full</option>%s
    </select>
    <button type="button" id="mint-button" class="btn-login">Create key</button>
  </form>
  <div id="mint-result" hidden></div>
  <div id="key-list" class="rank-list" role="region" aria-label="API keys">`,
		html.EscapeString(user.Username),
		notice,
		html.EscapeString(user.Username),
		adminBadge(user.IsAdmin),
		offboardScopeOption(user.IsAdmin),
	)
	for _, k := range keys {
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked"
		} else if !k.ExpiresAt.After(time.Now()) {
			status = "expired"
		}
		fmt.Fprintf(&b, `<div class="rank-row" data-key-id="%s">
  <span class="rank-name">%s <span class="rank-sub">%s · %s · created %s · expires %s</span></span>
  <span class="rank-metric">%s</span>`,
			html.EscapeString(k.ID),
			html.EscapeString(k.Name),
			html.EscapeString(k.Prefix),
			html.EscapeString(k.Scope),
			localTimeHTML(k.CreatedAt, "datetime"),
			localTimeHTML(k.ExpiresAt, "datetime"),
			html.EscapeString(status),
		)
		if status == "active" {
			fmt.Fprintf(&b, `<button type="button" class="btn-reject revoke-key" data-key-id="%s">Revoke</button>`, html.EscapeString(k.ID))
		}
		b.WriteString(`</div>`)
	}
	if len(keys) == 0 {
		b.WriteString(`<div class="rank-empty">No keys yet.</div>`)
	}
	b.WriteString(`</div>
</section>

<section>
  <h2 class="section-title">Your sites</h2>
  <p class="login-copy">Choose who can open each site, name its viewers, and manage the files it has uploaded. A new site opens only for you (or your team). Deploying and rolling back are done with the skill or MCP.</p>
  ` + usageHTML + `
  <div id="site-list" class="rank-list" role="region" aria-label="Sites"></div>
</section>
</main>`)
	b.WriteString(dashboardScript)
	b.WriteString(`</body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// offboardScopeOption offers the offboard scope to an admin only; the mint
// route refuses it to anyone else either way.
func offboardScopeOption(isAdmin bool) string {
	if isAdmin {
		return `
      <option value="offboard">Offboard (disable leavers)</option>`
	}
	return ""
}

// usageHTML is one line per namespace the person publishes in (their own,
// then each team's): sites and stored bytes against the quota. Best effort:
// a failed lookup leaves it out rather than failing the page.
func (h *DashboardHandler) usageHTML(r *http.Request, user *db.User) string {
	owners := []namespaceRef{{ID: user.ID, Name: user.Username}}
	teams, err := db.ListTeamsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("dashboard: list teams for %s: %v", user.Username, err)
		return ""
	}
	for _, team := range teams {
		owners = append(owners, namespaceRef{ID: team.ID, Name: team.Username})
	}
	usages, err := ownerUsages(r.Context(), h.database, h.quota, owners)
	if err != nil {
		log.Printf("dashboard: usage for %s: %v", user.Username, err)
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="rank-list" role="region" aria-label="Usage">`)
	for _, u := range usages {
		sites := formatCount(u.Sites) + " sites"
		if u.MaxSites > 0 {
			sites = formatCount(u.Sites) + " of " + formatCount(u.MaxSites) + " sites"
		}
		stored := formatBytes(uint64(u.Bytes)) + " stored"
		if u.MaxBytes > 0 {
			stored = formatBytes(uint64(u.Bytes)) + " of " + formatBytes(uint64(u.MaxBytes)) + " stored"
		}
		fmt.Fprintf(&b, `<div class="rank-row usage-row"><span class="rank-name">%s</span><span class="rank-metric">%s · %s</span></div>`,
			html.EscapeString(u.Owner), html.EscapeString(sites), html.EscapeString(stored))
	}
	b.WriteString(`</div>`)
	return b.String()
}

func adminBadge(isAdmin bool) string {
	if isAdmin {
		return ` <span class="chip">admin</span>`
	}
	return ""
}

// optionalUser resolves the session cookie without requiring one — the
// dashboard renders a sign-in prompt rather than a 401 when there is none.
// It intentionally does not accept X-API-Key: minting a key requires a
// session, so an agent presenting a key here should see the
// sign-in prompt, not a keys page it cannot act on.
func (h *DashboardHandler) optionalUser(r *http.Request) *db.User {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	verified, err := auth.VerifyBaseSessionCookie(h.signingKeys, c.Value)
	if err != nil {
		return nil
	}
	withUser, err := db.GetValidSession(r.Context(), h.database, verified.SessionID, h.sessionIdle)
	if err != nil || withUser.Session.UserID != verified.UserID {
		return nil
	}
	user := withUser.User
	return &user
}

// dashboardScript mints and revokes keys via fetch, same-origin, so the
// browser sends the session cookie and the same Origin header
// originCheckMiddleware requires. The plaintext key is shown exactly once,
// in a block shaped for the account-recovery skill's "paste this back to
// your agent" step.
const dashboardScript = `<script>
(function(){
  var mintButton = document.getElementById('mint-button');
  var nameInput = document.getElementById('key-name');
  var resultBox = document.getElementById('mint-result');
  var list = document.getElementById('key-list');
  if (!mintButton) return;

  mintButton.addEventListener('click', function(){
    mintButton.disabled = true;
    fetch('/api/keys', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({name: nameInput.value || '', scope: (document.getElementById('key-scope') || {}).value || 'publish'})
    }).then(function(r){ return r.json().then(function(body){ return {ok: r.ok, body: body}; }); })
      .then(function(res){
        mintButton.disabled = false;
        if (!res.ok) {
          resultBox.hidden = false;
          resultBox.textContent = 'Could not create key: ' + (res.body.error || 'unknown error');
          return;
        }
        var payload = JSON.stringify({api_key: res.body.api_key, username: document.querySelector('.mast').getAttribute('data-username') || ''});
        resultBox.hidden = false;
        resultBox.innerHTML = '<p><strong>Copy this now — it will not be shown again:</strong></p>' +
          '<pre style="white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);padding:12px;border-radius:4px">' +
          res.body.api_key + '</pre>' +
          '<p>Paste this block back to your agent:</p>' +
          '<pre style="white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);padding:12px;border-radius:4px">' +
          payload + '</pre>';
        location.reload();
      }).catch(function(){
        mintButton.disabled = false;
        resultBox.hidden = false;
        resultBox.textContent = 'Network error creating key.';
      });
  });

  if (list) {
    list.addEventListener('click', function(ev){
      var button = ev.target.closest('.revoke-key');
      if (!button) return;
      var id = button.getAttribute('data-key-id');
      if (!confirm('Revoke this key? Anything using it will stop working immediately.')) return;
      button.disabled = true;
      fetch('/api/keys/' + encodeURIComponent(id), {method: 'DELETE', credentials: 'same-origin'})
        .then(function(){ location.reload(); })
        .catch(function(){ button.disabled = false; });
    });
  }
})();
</script>` + dashboardSitesScript

// dashboardSitesScript renders the signed-in person's accessible sites and,
// for a site they own or belong to the owning team of (requireOwnerRole's
// gate), an access-level control, a viewer list and an asset list, each
// backed by the existing collaboration API — fetch-driven panels against
// the same endpoints the share dialog itself calls, not the dialog markup
// verbatim, since these live inline per site row rather than in one global
// modal.
//
// Every fetch to a site-management endpoint carries
// X-Simple-Host-Client: control-ui, which is what exempts a browser
// (rather than the skill or MCP) from the skill-version guard
// (notice_middleware.go's isClassifiedNonSkillClient) — without it every
// one of these calls would 400 with "skill_version_required".
const dashboardSitesScript = `<script>
(function(){
  var container = document.getElementById('site-list');
  if (!container) return;
  var CH = {'X-Simple-Host-Client': 'control-ui'};

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML; }
  function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    var units = ['KB','MB','GB'], u = -1;
    do { n = n / 1024; u++; } while (n >= 1024 && u < units.length - 1);
    return n.toFixed(1) + ' ' + units[u];
  }

  function loadSites() {
    fetch('/api/collaboration/sites', {credentials: 'same-origin', headers: CH})
      .then(function(r){ return r.json(); })
      .then(renderSites)
      .catch(function(){ container.innerHTML = '<div class="rank-empty">Could not load sites.</div>'; });
  }

  var LEVELS = [
    ['only_me', 'Only me (or my team)'],
    ['specific', 'Specific people or teams'],
    ['company', 'Anyone in the company with the link'],
    ['listed', 'Listed in the showcase and search'],
    ['network', 'Anyone on the network, no sign-in (needs admin approval)']
  ];
  function levelLabel(level) {
    for (var i = 0; i < LEVELS.length; i++) { if (LEVELS[i][0] === level) return LEVELS[i][1]; }
    return level || '';
  }

  // approvalProgress is " (1 of 2 approvals)" while a request needs more
  // than one admin, and nothing otherwise.
  function approvalProgress(req) {
    if (!req || !(req.approvals_required > 1)) return '';
    return ' (' + (req.approvals || 0) + ' of ' + req.approvals_required + ' approvals)';
  }

  function renderSites(sites) {
    if (!sites || !sites.length) { container.innerHTML = '<div class="rank-empty">No sites yet.</div>'; return; }
    container.innerHTML = '';
    sites.forEach(function(site){
      var canManage = site.access_role === 'owner' || site.access_role === 'member';
      var row = document.createElement('div');
      row.className = 'rank-row site-row';
      row.innerHTML = '<span class="rank-name">' + esc(site.owner_username) + '/' + esc(site.name) +
        ' <span class="rank-sub">' + esc(site.access_role) + ' · ' + esc(levelLabel(site.access)) +
        (site.network_request ? ' · network access requested' + approvalProgress(site.network_request) : '') + '</span></span>' +
        (canManage ? '<button type="button" class="btn-reject manage-toggle">Manage</button>' : '');
      var panel = document.createElement('div');
      panel.className = 'site-panel';
      panel.hidden = true;
      row.appendChild(panel);
      container.appendChild(row);
      if (!canManage) return;

      var toggle = row.querySelector('.manage-toggle');
      var loaded = false;
      toggle.addEventListener('click', function(){
        panel.hidden = !panel.hidden;
        if (!panel.hidden && !loaded) { loaded = true; renderPanel(panel, site); }
      });
    });
  }

  function renderPanel(panel, site) {
    var owner = site.owner_username, name = site.name;
    var options = LEVELS.map(function(l){
      return '<option value="' + l[0] + '"' + (l[0] === site.access ? ' selected' : '') + '>' + esc(l[1]) + '</option>';
    }).join('');
    panel.innerHTML =
      '<div class="site-subsection"><h4>Who can open it</h4>' +
      '<div class="add-row"><select class="access-select">' + options + '</select>' +
      '<button type="button" class="btn-login access-button">Save</button></div>' +
      '<p class="share-help access-status">' + (site.network_request ? 'Network access requested; waiting for ' + (site.network_request.approvals_required > 1 ? 'two admins' + approvalProgress(site.network_request) : 'an admin') + '. The site keeps its current level until then.' : '') + '</p></div>' +
      '<div class="site-subsection"><h4>Viewers</h4>' +
      '<p class="share-help">Named viewers can open the site while it is set to specific people or teams. Adding one sets that level.</p>' +
      '<div class="viewer-list" aria-live="polite"></div>' +
      '<div class="add-row"><input type="text" class="add-viewer-input" placeholder="username, another-username" autocomplete="off">' +
      '<button type="button" class="btn-login add-viewer-button">Add</button></div></div>' +
      '<div class="site-subsection"><h4>Assets</h4><div class="asset-list" aria-live="polite"></div></div>' +
      '<div class="site-subsection site-tabs"><div class="site-tab-buttons">' +
      '<button type="button" class="btn-reject site-tab-button active" data-tab="activity">Activity</button>' +
      '<button type="button" class="btn-reject site-tab-button" data-tab="visitors">Visitors</button>' +
      '</div>' +
      '<div class="site-tab-panel activity-tab"><div class="activity-list" aria-live="polite"></div></div>' +
      '<div class="site-tab-panel visitor-tab" hidden><div class="visitor-list" aria-live="polite"></div></div>' +
      '</div>';

    var base = '/api/collaboration/sites/' + encodeURIComponent(owner) + '/' + encodeURIComponent(name);
    var viewerList = panel.querySelector('.viewer-list');
    var assetList = panel.querySelector('.asset-list');
    var activityList = panel.querySelector('.activity-list');
    var visitorList = panel.querySelector('.visitor-list');
    var auditQuery = '/api/audit?owner=' + encodeURIComponent(owner) + '&site=' + encodeURIComponent(name);
    var accessQuery = '/api/access?owner=' + encodeURIComponent(owner) + '&site=' + encodeURIComponent(name);

    panel.querySelectorAll('.site-tab-button').forEach(function(button){
      button.addEventListener('click', function(){
        panel.querySelectorAll('.site-tab-button').forEach(function(b){ b.classList.remove('active'); });
        button.classList.add('active');
        var showActivity = button.getAttribute('data-tab') === 'activity';
        panel.querySelector('.activity-tab').hidden = !showActivity;
        panel.querySelector('.visitor-tab').hidden = showActivity;
      });
    });

    function loadActivity() {
      fetch(auditQuery, {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(body){
          var events = (body && body.events) || [];
          activityList.innerHTML = '';
          if (!events.length) { activityList.innerHTML = '<div class="rank-empty">No recorded actions yet.</div>'; return; }
          events.forEach(function(e){
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(e.action) +
              ' <span class="rank-sub">' + esc(new Date(e.at).toLocaleString()) + '</span></span>';
            activityList.appendChild(row);
          });
        })
        .catch(function(){ activityList.innerHTML = '<div class="rank-empty">Could not load activity.</div>'; });
    }

    function loadVisitors() {
      fetch(accessQuery, {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(body){
          visitorList.innerHTML = '';
          if (body && body.days) {
            // Counts only (ACCESS_LOG_VISIBILITY=counts): who viewed is for admins.
            if (!body.days.length) { visitorList.innerHTML = '<div class="rank-empty">No recorded visits in the last 30 days.</div>'; return; }
            var total = document.createElement('div');
            total.className = 'rank-row';
            total.innerHTML = '<span class="rank-name">' + esc(body.unique_viewers) + ' people viewed this site in the last 30 days</span>';
            visitorList.appendChild(total);
            body.days.forEach(function(d){
              var row = document.createElement('div');
              row.className = 'rank-row';
              row.innerHTML = '<span class="rank-name">' + esc(d.day) + ' <span class="rank-sub">' + esc(d.views) + ' views · ' + esc(d.unique_viewers) + ' people</span></span>';
              visitorList.appendChild(row);
            });
            return;
          }
          var entries = (body && body.entries) || [];
          if (!entries.length) { visitorList.innerHTML = '<div class="rank-empty">No recorded visits yet.</div>'; return; }
          entries.forEach(function(e){
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(e.method) + ' ' + esc(e.path) +
              ' <span class="rank-sub">' + esc(e.status) + ' · ' + esc(e.client_kind) + ' · ' + esc(new Date(e.at).toLocaleString()) + '</span></span>';
            visitorList.appendChild(row);
          });
        })
        .catch(function(){ visitorList.innerHTML = '<div class="rank-empty">Could not load visitors.</div>'; });
    }

    function loadViewers() {
      fetch(base + '/viewers', {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(viewers){
          viewerList.innerHTML = '';
          if (!viewers || !viewers.length) { viewerList.innerHTML = '<div class="rank-empty">No named viewers.</div>'; return; }
          viewers.forEach(function(v){
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(v.username) + ' <span class="rank-sub">' + esc(v.kind) + '</span></span>' +
              '<button type="button" class="btn-reject remove-viewer" data-username="' + esc(v.username) + '">Remove</button>';
            viewerList.appendChild(row);
          });
        })
        .catch(function(){ viewerList.innerHTML = '<div class="rank-empty">Could not load viewers.</div>'; });
    }

    function loadAssets() {
      fetch(base + '/assets', {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(assets){
          assetList.innerHTML = '';
          if (!assets || !assets.length) { assetList.innerHTML = '<div class="rank-empty">No assets uploaded.</div>'; return; }
          assets.forEach(function(a){
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name"><a href="' + esc(a.url) + '">' + esc(a.name) + '</a> <span class="rank-sub">' + esc(a.content_type) + ' · ' + fmtBytes(a.size) + '</span></span>' +
              '<button type="button" class="btn-reject remove-asset" data-id="' + esc(a.id) + '">Delete</button>';
            assetList.appendChild(row);
          });
        })
        .catch(function(){ assetList.innerHTML = '<div class="rank-empty">Could not load assets.</div>'; });
    }

    panel.querySelector('.access-button').addEventListener('click', function(){
      var level = panel.querySelector('.access-select').value;
      var payload = {level: level};
      if (level === 'network') {
        var reason = prompt('Anyone who can reach this server will be able to open the site without signing in. An admin has to approve this. Why does it need to be open?');
        if (!reason) return;
        payload.reason = reason;
      }
      fetch(base + '/access', {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json'}, CH),
        body: JSON.stringify(payload),
      }).then(function(r){
        return r.json().then(function(b){
          if (!r.ok) { alert('Could not change access: ' + (b.error || 'unknown error')); return; }
          panel.querySelector('.access-status').textContent = b.note || '';
          loadSites();
        });
      }).catch(function(){ alert('Network error changing access.'); });
    });

    panel.querySelector('.add-viewer-button').addEventListener('click', function(){
      var input = panel.querySelector('.add-viewer-input');
      var usernames = input.value.split(',').map(function(s){ return s.trim(); }).filter(Boolean);
      if (!usernames.length) return;
      fetch(base + '/viewers', {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json'}, CH),
        body: JSON.stringify({usernames: usernames}),
      }).then(function(r){
        if (!r.ok) return r.json().then(function(b){ alert('Could not add: ' + (b.error || 'unknown error')); });
        input.value = '';
        loadViewers();
      }).catch(function(){ alert('Network error adding viewers.'); });
    });

    viewerList.addEventListener('click', function(ev){
      var button = ev.target.closest('.remove-viewer');
      if (!button) return;
      fetch(base + '/viewers/' + encodeURIComponent(button.getAttribute('data-username')), {method: 'DELETE', credentials: 'same-origin', headers: CH})
        .then(loadViewers)
        .catch(function(){ alert('Network error removing viewer.'); });
    });

    assetList.addEventListener('click', function(ev){
      var button = ev.target.closest('.remove-asset');
      if (!button) return;
      if (!confirm('Delete this asset? Any page still linking to it will break.')) return;
      fetch(base + '/assets/' + encodeURIComponent(button.getAttribute('data-id')), {method: 'DELETE', credentials: 'same-origin', headers: CH})
        .then(loadAssets)
        .catch(function(){ alert('Network error deleting asset.'); });
    });

    loadViewers();
    loadAssets();
    loadActivity();
    loadVisitors();
  }

  loadSites();
})();
</script>`
