package handler

import (
	"database/sql"
	"fmt"
	"github.com/vsriram/simple-host/internal/oplimits"
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
	// hosts validates ?switch= (a site's "Switch account" link).
	hosts HostModel
}

// WithHosts sets the host model the switch-account notice checks its
// address against.
func (h *DashboardHandler) WithHosts(hosts HostModel) *DashboardHandler {
	h.hosts = hosts
	return h
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
	if target, ok := switchTarget(h.hosts, r.URL.Query().Get("switch")); ok {
		// Arrived from a site's "Switch account" link: signing out here ends
		// this session everywhere and goes back to the site, which asks for
		// sign-in again.
		notice = `<section class="roadmap-block"><span class="roadmap-tag">Switch account</span>
  <span class="roadmap-text">You're signed in as ` + html.EscapeString(user.Username) + `. To open ` + html.EscapeString(strings.TrimSuffix(strings.TrimPrefix(target, "https://"), "/")) + ` with a different account, sign out, then sign in with that account.</span>
  <form method="POST" action="/auth/logout"><input type="hidden" name="to" value="` + html.EscapeString(target) + `"><button type="submit" class="btn-logout">Sign out and switch</button></form>
</section>`
	} else if r.URL.Query().Get("notice") == "username_suffixed" {
		notice = `<section class="roadmap-block"><span class="roadmap-tag">Note</span>
  <span class="roadmap-text">Your usual username was already taken, so your account and site address use a suffixed version instead.</span>
</section>`
	}

	fmt.Fprintf(&b, oplimits.Expand(`<header class="bar">
  <div class="mast" data-username="%s">Simple Host<span class="dot">.</span> <span class="kicker">dashboard</span></div>
  <nav class="dash-nav"><a href="/dashboard">Keys</a> <span class="sep" aria-hidden="true"></span> <a href="/auth/sessions">Sessions</a></nav>
  <form method="POST" action="/auth/logout" class="logout-form"><button type="submit" class="btn-logout">Sign out</button></form>
</header>
<main>%s
<h2 class="section-title">Signed in as %s%s</h2>

<section>
  <h2 class="section-title">API keys</h2>
  <p class="login-copy">A key authenticates CI or other automation as you. Mint one per job or machine so each can be revoked without touching the others. Keys expire after {{API_KEY_DEFAULT_DAYS}} days; mint a fresh one when yours does. A publish key can deploy, update and roll back your sites and use their saved data and files; a full key can do everything you can, except administration.</p>
  <form id="mint-form" class="login-form" onsubmit="return false">
    <input type="text" id="key-name" placeholder="Name (e.g. laptop, CI)" maxlength="200" autocomplete="off">
    <select id="key-scope" aria-label="What the key can do">
      <option value="publish" selected>Publish</option>
      <option value="full">Full</option>%s
    </select>
    <button type="button" id="mint-button" class="btn-login">Create key</button>
  </form>
  <div id="mint-result" hidden></div>
  <div id="key-list" class="rank-list" role="region" aria-label="API keys">`),
		html.EscapeString(user.Username),
		notice,
		html.EscapeString(user.Username),
		adminBadge(user.IsAdmin),
		offboardScopeOption(user.IsAdmin),
	)
	for _, k := range keys {
		status := keyStatus(k, time.Now())
		fmt.Fprintf(&b, `<div class="rank-row" data-key-id="%s">
  <span class="rank-name">%s <span class="rank-sub">%s · %s · created %s · expires %s · %s</span></span>
  <span class="rank-metric">%s</span>`,
			html.EscapeString(k.ID),
			html.EscapeString(k.Name),
			html.EscapeString(keyLabel(k.Last4)),
			html.EscapeString(k.Scope),
			localTimeHTML(k.CreatedAt, "datetime"),
			localTimeHTML(k.ExpiresAt, "datetime"),
			keyLastUsed(k),
			html.EscapeString(status),
		)
		if status == "active" || status == "expires soon" {
			fmt.Fprintf(&b, `<button type="button" class="btn-reject revoke-key" data-key-id="%s">Revoke</button>`, html.EscapeString(k.ID))
		}
		b.WriteString(`</div>`)
	}
	if len(keys) == 0 {
		b.WriteString(`<div class="rank-empty">No keys yet.</div>`)
	}
	b.WriteString(`</div>
</section>

` + idleNoticeHTML(r.Context(), h.database, user) + `
<section>
  <h2 class="section-title">Your sites</h2>
  <p class="login-copy">Open each site, choose who can open it, name its viewers, manage the files it has uploaded, make an earlier version live, restore its saved data, download it, rename it, hand it to a team, or delete it. A new site opens only for you (or your team). Publishing is done by your AI app.</p>
  ` + usageHTML + oplimits.Expand(`
  <div id="site-list" class="rank-list" role="region" aria-label="Sites"></div>
</section>

<section id="teams-section">
  <h2 class="section-title">Teams</h2>
  <p class="login-copy">A team publishes sites together: every member can change or delete any of its sites. Add people by username or work email; someone who hasn't signed in yet joins at their first sign-in.</p>
  <form class="login-form" onsubmit="return false">
    <input type="text" id="team-name" placeholder="New team name" maxlength="58" autocomplete="off">
    <button type="button" id="team-create" class="btn-login">Create team</button>
  </form>
  <div id="team-list" class="rank-list" role="region" aria-label="Teams"></div>
</section>

<section id="shared-section" hidden>
  <h2 class="section-title">Shared with me</h2>
  <p class="login-copy">Sites other people have shared with you by name, or with a team you are in.</p>
  <div id="shared-list" class="rank-list" role="region" aria-label="Sites shared with me"></div>
</section>

<section id="deleted-section" hidden>
  <h2 class="section-title">Recently deleted</h2>
  <p class="login-copy">A deleted site stays here for {{DELETED_RETENTION}}, and its name stays yours. Restore brings it back as it was: its files, saved data, who can open it, its viewers and uploaded files.</p>
  <div id="deleted-list" class="rank-list" role="region" aria-label="Recently deleted sites"></div>
</section>
</main>`))
	b.WriteString(oplimits.Expand(dashboardScript))
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
// your agent" step, and stays on screen until dismissed: minting never
// reloads the page (the new row is added in place).
const dashboardScript = `<script>
(function(){
  var mintButton = document.getElementById('mint-button');
  var nameInput = document.getElementById('key-name');
  var resultBox = document.getElementById('mint-result');
  var list = document.getElementById('key-list');
  if (!mintButton) return;

  function block(text) {
    var pre = document.createElement('pre');
    pre.style.cssText = 'white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);padding:12px;border-radius:4px';
    pre.textContent = text;
    return pre;
  }
  function para(text, strong) {
    var p = document.createElement('p');
    if (strong) { var s = document.createElement('strong'); s.textContent = text; p.appendChild(s); } else { p.textContent = text; }
    return p;
  }

  // showMinted puts the new key and the paste-back block in the result box
  // until "Done" is pressed.
  function showMinted(key, payload) {
    resultBox.innerHTML = '';
    resultBox.appendChild(para('Copy this now. It will not be shown again:', true));
    resultBox.appendChild(block(key));
    resultBox.appendChild(para('Paste this block back to your agent:'));
    resultBox.appendChild(block(payload));
    var done = document.createElement('button');
    done.type = 'button';
    done.className = 'btn-reject';
    done.id = 'mint-done';
    done.textContent = 'Done, I have copied it';
    done.addEventListener('click', function(){ resultBox.hidden = true; resultBox.innerHTML = ''; });
    resultBox.appendChild(done);
    resultBox.hidden = false;
  }

  // addKeyRow adds the minted key to the top of the list, shaped like the
  // rows the server renders.
  function addKeyRow(k) {
    var empty = list.querySelector('.rank-empty');
    if (empty) empty.remove();
    var row = document.createElement('div');
    row.className = 'rank-row';
    row.setAttribute('data-key-id', k.id);
    var name = document.createElement('span');
    name.className = 'rank-name';
    name.textContent = k.name + ' ';
    var sub = document.createElement('span');
    sub.className = 'rank-sub';
    sub.textContent = (k.last4 ? 'ends …' + k.last4 : 'earlier key') + ' · ' + k.scope + ' · created ' + new Date(k.created_at).toLocaleString() + ' · expires ' + new Date(k.expires_at).toLocaleString() + ' · never used';
    name.appendChild(sub);
    var status = document.createElement('span');
    status.className = 'rank-metric';
    status.textContent = 'active';
    var revoke = document.createElement('button');
    revoke.type = 'button';
    revoke.className = 'btn-reject revoke-key';
    revoke.setAttribute('data-key-id', k.id);
    revoke.textContent = 'Revoke';
    row.appendChild(name);
    row.appendChild(status);
    row.appendChild(revoke);
    list.insertBefore(row, list.firstChild);
  }

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
        // The key is shown once and never again, so the page does not
        // reload here: it stays until the person dismisses it, and the new
        // key joins the list in place.
        showMinted(res.body.api_key, payload);
        addKeyRow(res.body);
        nameInput.value = '';
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
</script>` + dashboardSitesScript + dashboardDeletedScript + dashboardTeamsScript

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

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
  function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    var units = ['KB','MB','GB'], u = -1;
    do { n = n / 1024; u++; } while (n >= 1024 && u < units.length - 1);
    return n.toFixed(1) + ' ' + units[u];
  }

  function loadSites() {
    fetch('/api/collaboration/sites?include=shared', {credentials: 'same-origin', headers: CH})
      .then(function(r){ return r.json(); })
      .then(function(all){
        all = all || [];
        renderShared(all.filter(function(s){ return s.access_role === 'viewer'; }));
        renderSites(all.filter(function(s){ return s.access_role !== 'viewer'; }));
      })
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

  // decisionNote is the last admin decision about who can open the site,
  // shown until the owner acts on it (see access_decision in the API).
  function decisionNote(site) {
    var d = site.access_decision;
    if (!d) return '';
    var when = new Date(d.at).toLocaleString();
    var why = d.reason ? ': "' + d.reason + '"' : '.';
    if (d.decision === 'restricted') return 'An admin restricted this site to only you (or your team) on ' + when + why + ' Until an admin lifts it, its level cannot be raised and viewers cannot be added; ask an admin if it should open again.';
    if (d.decision === 'declined') return 'An admin declined network access on ' + when + why + ' You can ask again with a new reason.';
    if (d.decision === 'revoked') return 'An admin took this site off the network on ' + when + why + ' You can ask again with a new reason.';
    return '';
  }
  function decisionShort(site) {
    var d = site.access_decision;
    if (!d) return '';
    return {restricted: ' · restricted by an admin', declined: ' · network access declined', revoked: ' · network access revoked'}[d.decision] || '';
  }

  // safeURL keeps only http(s) addresses as link targets.
  function safeURL(u) { return /^https?:\/\//i.test(u || '') ? u : '#'; }
  function when(s) { var d = new Date(s); return isNaN(d) ? '' : d.toLocaleString(); }
  function liveLine(site) {
    var v = site.active_version > 0 ? 'version ' + site.active_version + ' live' : 'nothing published';
    return v + ' · updated ' + when(site.updated_at);
  }

  // renderFirstRun is the empty state: sites come from connecting an AI
  // app, so the one step that does that is shown in place of an empty list.
  function renderFirstRun() {
    container.innerHTML = '';
    var box = document.createElement('div');
    box.className = 'first-run';
    var lead = document.createElement('p');
    lead.className = 'login-copy';
    lead.textContent = 'No sites yet. Your sites come from your AI app: add this server to ChatGPT, Claude, Copilot, Cursor or Codex, sign in with your work account when it asks, then ask it to publish something.';
    var pre = document.createElement('pre');
    pre.className = 'mcp-address';
    pre.style.cssText = 'white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);padding:12px;border-radius:4px';
    pre.textContent = location.origin + '/mcp';
    var more = document.createElement('p');
    more.className = 'login-copy';
    more.appendChild(document.createTextNode('Or install the plugin, which adds the server and the skills in one step: '));
    var zip = document.createElement('a');
    zip.href = '/plugin.zip';
    zip.setAttribute('download', '');
    zip.textContent = 'Download plugin.zip';
    more.appendChild(zip);
    more.appendChild(document.createTextNode('. Every other way to connect is on the '));
    var inst = document.createElement('a');
    inst.href = '/install.html';
    inst.textContent = 'install page';
    more.appendChild(inst);
    more.appendChild(document.createTextNode('.'));
    box.appendChild(lead);
    box.appendChild(pre);
    box.appendChild(more);
    container.appendChild(box);
  }

  // renderShared lists the sites shared with the person: open only.
  function renderShared(sites) {
    var section = document.getElementById('shared-section');
    var list = document.getElementById('shared-list');
    if (!section || !list) return;
    section.hidden = !sites.length;
    list.innerHTML = sites.map(function(site){
      var via = site.shared_via ? 'shared with ' + site.shared_via : 'shared with you';
      return '<div class="rank-row"><span class="rank-name"><a href="' + esc(safeURL(site.url)) + '">' + esc(site.owner_username) + '/' + esc(site.name) + '</a>' +
        ' <span class="rank-sub">' + esc(via) + ' · updated ' + esc(when(site.updated_at)) + '</span></span></div>';
    }).join('');
  }

  function renderSites(sites) {
    if (!sites || !sites.length) { renderFirstRun(); return; }
    container.innerHTML = '';
    sites.forEach(function(site){
      var canManage = site.access_role === 'owner' || site.access_role === 'member';
      var row = document.createElement('div');
      row.className = 'rank-row site-row';
      row.innerHTML = '<span class="rank-name"><a href="' + esc(safeURL(site.url)) + '">' + esc(site.owner_username) + '/' + esc(site.name) + '</a>' +
        ' <span class="rank-sub">' + esc(site.access_role) + ' · ' + esc(levelLabel(site.access)) +
        (site.network_request ? ' · network access requested' + approvalProgress(site.network_request) : esc(decisionShort(site))) +
        ' · ' + esc(liveLine(site)) + '</span></span>' +
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
    // An admin's restriction is lifted only by an admin, so the controls
    // that would open the site up are off while it stands.
    var locked = site.access_decision && site.access_decision.decision === 'restricted' ? ' disabled' : '';
    var options = LEVELS.map(function(l){
      return '<option value="' + l[0] + '"' + (l[0] === site.access ? ' selected' : '') + '>' + esc(l[1]) + '</option>';
    }).join('');
    panel.innerHTML =
      '<div class="site-subsection"><h4>Who can open it</h4>' +
      '<div class="add-row"><select class="access-select"' + locked + '>' + options + '</select>' +
      '<button type="button" class="btn-login access-button"' + locked + '>Save</button></div>' +
      '<p class="share-help access-status">' + (site.network_request ? 'Network access requested; waiting for ' + (site.network_request.approvals_required > 1 ? 'two admins' + approvalProgress(site.network_request) : 'an admin') + '. The site keeps its current level until then.' : esc(decisionNote(site))) + '</p></div>' +
      '<div class="site-subsection"><h4>Viewers</h4>' +
      '<p class="share-help">Named viewers can open the site while it is set to specific people or teams. Adding one sets that level. Someone who hasn\'t signed in yet can be added by work email; they can open the site after their first sign-in.</p>' +
      '<div class="viewer-list" aria-live="polite"></div>' +
      '<div class="add-row"><input type="text" class="add-viewer-input" placeholder="username or work email, another" autocomplete="off"' + locked + '>' +
      '<button type="button" class="btn-login add-viewer-button"' + locked + '>Add</button></div></div>' +
      '<div class="site-subsection"><h4>Assets</h4><div class="asset-list" aria-live="polite"></div></div>' +
      '<div class="site-subsection"><h4>Versions</h4>' +
      '<p class="share-help">Preview opens a version in a new tab, for you and your team only, for {{PREVIEW_LINK_TTL}}. Make live shows it to visitors at once; the other versions stay here.</p>' +
      '<div class="version-list" aria-live="polite"></div></div>' +
      '<div class="site-subsection"><h4>Saved data</h4>' +
      '<p class="share-help">The last versions of what the site\'s pages have saved. Restore puts an earlier one back as the current saved data; the data it replaces stays in this list.</p>' +
      '<div class="state-list" aria-live="polite"></div></div>' +
      '<div class="site-subsection"><h4>Download</h4>' +
      '<p class="share-help">One zip of the live files, the current saved data and its history, the version list and the uploaded files.</p>' +
      '<div class="add-row"><button type="button" class="btn-login download-button">Download site</button></div></div>' +
      '<div class="site-subsection"><h4>Rename</h4>' +
      '<p class="share-help">The site gets a new address. The old one sends visitors on to it until another site takes the name.</p>' +
      '<div class="add-row"><input type="text" class="rename-input" placeholder="new-name" autocomplete="off"' + locked + '>' +
      '<button type="button" class="btn-login rename-button"' + locked + '>Rename</button></div></div>' +
      '<div class="site-subsection"><h4>Move to a team</h4>' +
      '<p class="share-help">Move the site into a team you are in. Its files, versions, saved data, uploads, access and viewers go with it, and the old address sends visitors on to the new one. Network access has to be requested again.</p>' +
      '<div class="add-row"><input type="text" class="transfer-input" placeholder="team-name" autocomplete="off"' + locked + '>' +
      '<button type="button" class="btn-login transfer-button"' + locked + '>Move</button></div></div>' +
      '<div class="site-subsection"><h4>Delete</h4>' +
      '<p class="share-help">The site stops being served at once. It stays in Recently deleted for {{DELETED_RETENTION}} with its files, saved data, viewers and uploads, and can be restored from there.</p>' +
      '<div class="add-row"><button type="button" class="btn-reject delete-site-button">Delete site</button></div></div>' +
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
    var versionList = panel.querySelector('.version-list');
    var stateList = panel.querySelector('.state-list');
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
              ' <span class="rank-sub">' + (e.actor_name ? esc(e.actor_name) + ' · ' : '') + esc(new Date(e.at).toLocaleString()) + '</span></span>';
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
            row.innerHTML = '<span class="rank-name">' + esc(v.username) + ' <span class="rank-sub">' + (v.pending ? 'hasn\'t signed in yet' : esc(v.kind)) + '</span></span>' +
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

    function moveSite(path, payload, confirmText) {
      if (!confirm(confirmText)) return;
      fetch(base + path, {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json'}, CH),
        body: JSON.stringify(payload),
      }).then(function(r){
        return r.json().then(function(b){
          if (!r.ok) { alert('Could not move the site: ' + (b.error || 'unknown error')); return; }
          alert(b.owner + '/' + b.name + ' is now at ' + b.url + '\nThe old address sends visitors there.');
          loadSites();
        });
      }).catch(function(){ alert('Network error moving the site.'); });
    }

    panel.querySelector('.rename-button').addEventListener('click', function(){
      var name = panel.querySelector('.rename-input').value.trim();
      if (!name) return;
      moveSite('/rename', {name: name}, 'Rename ' + site.name + ' to ' + name + '? Its address changes; the old one redirects.');
    });

    panel.querySelector('.transfer-button').addEventListener('click', function(){
      var to = panel.querySelector('.transfer-input').value.trim();
      if (!to) return;
      moveSite('/transfer', {to: to}, 'Move ' + owner + '/' + site.name + ' into ' + to + '? Every member of that team can then change or delete it.');
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

    function loadVersions() {
      fetch(base + '/versions', {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(versions){
          versionList.innerHTML = '';
          if (!versions || !versions.length) { versionList.innerHTML = '<div class="rank-empty">No versions yet.</div>'; return; }
          versions.forEach(function(v){
            var live = v.version_number === site.active_version;
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">Version ' + esc(v.version_number) +
              ' <span class="rank-sub">' + esc(when(v.created_at)) + (v.uploaded_by ? ' · by ' + esc(v.uploaded_by) : '') + '</span></span>' +
              '<button type="button" class="btn-reject preview-version" data-version="' + esc(v.version_number) + '">Preview</button>' +
              (live ? ' <span class="rank-metric">live</span>' :
                ' <button type="button" class="btn-login make-live" data-version="' + esc(v.version_number) + '">Make live</button>');
            versionList.appendChild(row);
          });
        })
        .catch(function(){ versionList.innerHTML = '<div class="rank-empty">Could not load versions.</div>'; });
    }

    function loadStateVersions() {
      fetch(base + '/state-versions', {credentials: 'same-origin', headers: CH})
        .then(function(r){ return r.json(); })
        .then(function(entries){
          stateList.innerHTML = '';
          if (!entries || !entries.length) { stateList.innerHTML = '<div class="rank-empty">Nothing saved yet.</div>'; return; }
          entries.forEach(function(e, i){
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">Saved data version ' + esc(e.version) +
              ' <span class="rank-sub">' + esc(when(e.created_at)) + (e.written_by ? ' · by ' + esc(e.written_by) : '') + ' · ' + fmtBytes(e.bytes || 0) + '</span></span>' +
              (i === 0 ? '<span class="rank-metric">current</span>' :
                '<button type="button" class="btn-login restore-state" data-id="' + esc(e.id) + '" data-version="' + esc(e.version) + '">Restore</button>');
            stateList.appendChild(row);
          });
        })
        .catch(function(){ stateList.innerHTML = '<div class="rank-empty">Could not load saved data history.</div>'; });
    }

    versionList.addEventListener('click', function(ev){
      var preview = ev.target.closest('.preview-version');
      if (preview) {
        // Opened before the request so the browser treats it as the click's
        // own window, then pointed at the link once it arrives.
        var win = window.open('', '_blank');
        fetch(base + '/versions/' + encodeURIComponent(preview.getAttribute('data-version')) + '/preview', {credentials: 'same-origin', headers: CH})
          .then(function(r){ return r.json().then(function(b){
            if (!r.ok) { if (win) win.close(); alert('Could not open the preview: ' + (b.error || 'unknown error')); return; }
            if (win) { win.opener = null; win.location = b.url; } else { location.href = b.url; }
          }); })
          .catch(function(){ if (win) win.close(); alert('Network error opening the preview.'); });
        return;
      }
      var button = ev.target.closest('.make-live');
      if (!button) return;
      var version = parseInt(button.getAttribute('data-version'), 10);
      if (!confirm('Make version ' + version + ' of ' + site.name + ' live? Visitors see it at once.')) return;
      button.disabled = true;
      fetch(base + '/rollback', {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json', 'If-Match': site.etag}, CH),
        body: JSON.stringify({version: version}),
      }).then(function(r){
        return r.json().then(function(b){
          if (!r.ok) {
            alert(r.status === 412 ? 'The site changed since this page loaded. Reload and try again.' : 'Could not make it live: ' + (b.error || 'unknown error'));
            button.disabled = false;
            return;
          }
          site.active_version = b.active_version;
          site.etag = b.etag;
          loadVersions();
          loadSites();
        });
      }).catch(function(){ alert('Network error changing the live version.'); button.disabled = false; });
    });

    stateList.addEventListener('click', function(ev){
      var button = ev.target.closest('.restore-state');
      if (!button) return;
      if (!confirm('Restore saved data version ' + button.getAttribute('data-version') + '? It replaces what the pages have saved now; the data it replaces stays in this list.')) return;
      button.disabled = true;
      fetch(base + '/state-versions/' + encodeURIComponent(button.getAttribute('data-id')) + '/restore', {method: 'POST', credentials: 'same-origin', headers: CH})
        .then(function(r){
          return r.json().then(function(b){
            if (!r.ok) { alert('Could not restore: ' + (b.error || 'unknown error')); button.disabled = false; return; }
            loadStateVersions();
          });
        })
        .catch(function(){ alert('Network error restoring saved data.'); button.disabled = false; });
    });

    panel.querySelector('.download-button').addEventListener('click', function(){
      var button = this;
      button.disabled = true;
      fetch(base + '/export-link', {method: 'POST', credentials: 'same-origin', headers: CH})
        .then(function(r){
          return r.json().then(function(b){
            button.disabled = false;
            if (!r.ok || !b.url) { alert('Could not prepare the download: ' + (b.error || 'unknown error')); return; }
            location.href = b.url;
          });
        })
        .catch(function(){ alert('Network error preparing the download.'); button.disabled = false; });
    });

    panel.querySelector('.delete-site-button').addEventListener('click', function(){
      var typed = prompt('Delete ' + owner + '/' + name + '? It stops being served at once and stays in Recently deleted for {{DELETED_RETENTION}}. Type the site name to confirm:');
      if (typed === null) return;
      if (typed.trim() !== name) { alert('The name did not match, so nothing was deleted.'); return; }
      fetch(base, {method: 'DELETE', credentials: 'same-origin', headers: CH})
        .then(function(r){
          if (r.ok) { location.reload(); return; }
          return r.json().then(function(b){ alert('Could not delete: ' + (b.error || 'unknown error')); });
        })
        .catch(function(){ alert('Network error deleting the site.'); });
    });

    loadVersions();
    loadStateVersions();
    loadViewers();
    loadAssets();
    loadActivity();
    loadVisitors();
  }

  loadSites();
})();
</script>`

// dashboardDeletedScript fills the "Recently deleted" section from
// GET /api/deleted-sites (the person's own and their teams'), and restores
// one through the owner-qualified restore route. The section stays hidden
// while nothing is recently deleted.
const dashboardDeletedScript = `<script>
(function(){
  var section = document.getElementById('deleted-section');
  var list = document.getElementById('deleted-list');
  if (!section || !list) return;
  var CH = {'X-Simple-Host-Client': 'control-ui'};
  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
  function when(s) { var d = new Date(s); return isNaN(d) ? '' : d.toLocaleString(); }

  function load() {
    fetch('/api/deleted-sites', {credentials: 'same-origin', headers: CH})
      .then(function(r){ return r.ok ? r.json() : []; })
      .then(render)
      .catch(function(){});
  }

  function render(sites) {
    if (!sites || !sites.length) { section.hidden = true; list.innerHTML = ''; return; }
    section.hidden = false;
    list.innerHTML = sites.map(function(s){
      var by = s.deleted_by ? 'deleted by ' + esc(s.deleted_by) + ' ' : 'deleted ';
      return '<div class="rank-row"><span class="rank-name">' + esc(s.owner) + '/' + esc(s.site) +
        ' <span class="rank-sub">' + by + esc(when(s.deleted_at)) + ' · restorable until ' + esc(when(s.restorable_until)) + '</span></span>' +
        '<button type="button" class="btn-reset restore-site" data-owner="' + esc(s.owner) + '" data-site="' + esc(s.site) + '">Restore</button></div>';
    }).join('');
  }

  list.addEventListener('click', function(ev){
    var button = ev.target.closest('.restore-site');
    if (!button) return;
    button.disabled = true;
    var path = '/api/collaboration/sites/' + encodeURIComponent(button.getAttribute('data-owner')) + '/' +
      encodeURIComponent(button.getAttribute('data-site')) + '/restore';
    fetch(path, {method: 'POST', credentials: 'same-origin', headers: CH})
      .then(function(r){
        if (r.ok) { location.reload(); return; }
        return r.json().then(function(body){ alert((body && body.error) || 'Could not restore the site.'); button.disabled = false; });
      })
      .catch(function(){ alert('Network error restoring the site.'); button.disabled = false; });
  });

  load();
})();
</script>`

// dashboardTeamsScript lists the person's teams (GET /api/teams) and, per
// team, its members with add and remove, Leave and Delete, each over the
// team routes. Leaving as the last active member and deleting a team with
// sites both need the team's name typed back (confirm_name), which the page
// asks for before sending.
const dashboardTeamsScript = `<script>
(function(){
  var list = document.getElementById('team-list');
  var createButton = document.getElementById('team-create');
  var nameInput = document.getElementById('team-name');
  if (!list) return;
  var CH = {'X-Simple-Host-Client': 'control-ui'};
  var me = (document.querySelector('.mast') || {getAttribute: function(){ return ''; }}).getAttribute('data-username') || '';
  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
  function teamPath(team) { return '/api/teams/' + encodeURIComponent(team); }
  function send(method, path, body) {
    var opts = {method: method, credentials: 'same-origin', headers: Object.assign({}, CH)};
    if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
    return fetch(path, opts).then(function(r){
      return r.text().then(function(t){ var b = {}; try { b = t ? JSON.parse(t) : {}; } catch (e) {} return {ok: r.ok, status: r.status, body: b}; });
    });
  }

  function load() {
    send('GET', '/api/teams').then(function(res){
      var teams = (res.body && res.body.teams) || [];
      list.innerHTML = '';
      if (!teams.length) { list.innerHTML = '<div class="rank-empty">You are not in a team.</div>'; return; }
      teams.forEach(renderTeam);
    }).catch(function(){ list.innerHTML = '<div class="rank-empty">Could not load teams.</div>'; });
  }

  function renderTeam(team) {
    var row = document.createElement('div');
    row.className = 'rank-row site-row';
    row.innerHTML = '<span class="rank-name">' + esc(team.name) + '</span>' +
      '<button type="button" class="btn-reject manage-toggle">Manage</button>';
    var panel = document.createElement('div');
    panel.className = 'site-panel';
    panel.hidden = true;
    row.appendChild(panel);
    list.appendChild(row);
    var loaded = false;
    row.querySelector('.manage-toggle').addEventListener('click', function(){
      panel.hidden = !panel.hidden;
      if (!panel.hidden && !loaded) { loaded = true; renderPanel(panel, team.name); }
    });
  }

  // confirmDestroy asks for the team's name typed back when the server says
  // the change deletes the team and its sites, then repeats it with it.
  function confirmDestroy(res, name, retry) {
    var typed = prompt((res.body.error ? res.body.error.split(' Confirm with')[0] + '. ' : '') + 'Type the team name to confirm:');
    if (typed === null) return;
    if (typed.trim() !== name) { alert('The name did not match, so nothing changed.'); return; }
    retry(name);
  }

  function renderPanel(panel, name) {
    panel.innerHTML =
      '<div class="site-subsection"><h4>Members</h4><div class="member-list" aria-live="polite"></div>' +
      '<div class="add-row"><input type="text" class="add-member-input" placeholder="username or work email, another" autocomplete="off">' +
      '<button type="button" class="btn-login add-member-button">Add</button></div></div>' +
      '<div class="site-subsection"><h4>Leave or delete</h4>' +
      '<p class="share-help">Leaving keeps the team and its sites for the others. If you are the last active member, leaving deletes the team and every site it owns. Deleting removes the team and all its sites for good; move any site you want to keep into another team first.</p>' +
      '<div class="add-row"><button type="button" class="btn-reject leave-team">Leave team</button>' +
      '<button type="button" class="btn-reject delete-team">Delete team</button></div></div>';
    var memberList = panel.querySelector('.member-list');

    function renderMembers(members) {
      memberList.innerHTML = '';
      (members || []).forEach(function(m){
        var r = document.createElement('div');
        r.className = 'rank-row';
        var sub = m.pending ? 'hasn\'t signed in yet' : (m.username === me ? 'you' : 'member');
        r.innerHTML = '<span class="rank-name">' + esc(m.username) + ' <span class="rank-sub">' + esc(sub) + '</span></span>' +
          (m.username === me ? '' : '<button type="button" class="btn-reject remove-member" data-username="' + esc(m.username) + '">Remove</button>');
        memberList.appendChild(r);
      });
    }
    function loadMembers() {
      send('GET', teamPath(name) + '/members').then(function(res){
        if (!res.ok) { memberList.innerHTML = '<div class="rank-empty">Could not load members.</div>'; return; }
        renderMembers(res.body.members);
      }).catch(function(){ memberList.innerHTML = '<div class="rank-empty">Could not load members.</div>'; });
    }

    panel.querySelector('.add-member-button').addEventListener('click', function(){
      var input = panel.querySelector('.add-member-input');
      var usernames = input.value.split(',').map(function(s){ return s.trim(); }).filter(Boolean);
      if (!usernames.length) return;
      send('POST', teamPath(name) + '/members', {usernames: usernames}).then(function(res){
        if (!res.ok) { alert('Could not add: ' + (res.body.error || 'unknown error')); return; }
        input.value = '';
        renderMembers(res.body.members);
      }).catch(function(){ alert('Network error adding members.'); });
    });

    memberList.addEventListener('click', function(ev){
      var button = ev.target.closest('.remove-member');
      if (!button) return;
      var who = button.getAttribute('data-username');
      if (!confirm('Remove ' + who + ' from ' + name + '? They lose access to the team\'s sites.')) return;
      send('DELETE', teamPath(name) + '/members/' + encodeURIComponent(who)).then(function(res){
        if (!res.ok) { alert('Could not remove: ' + (res.body.error || 'unknown error')); return; }
        loadMembers();
      }).catch(function(){ alert('Network error removing the member.'); });
    });

    function leave(confirmName) {
      var path = teamPath(name) + '/leave' + (confirmName ? '?confirm_name=' + encodeURIComponent(confirmName) : '');
      send('POST', path).then(function(res){
        if (res.status === 409 && res.body.code === 'confirm_team_delete') { confirmDestroy(res, name, leave); return; }
        if (!res.ok) { alert('Could not leave: ' + (res.body.error || 'unknown error')); return; }
        location.reload();
      }).catch(function(){ alert('Network error leaving the team.'); });
    }
    panel.querySelector('.leave-team').addEventListener('click', function(){
      if (!confirm('Leave ' + name + '? You lose access to its sites.')) return;
      leave('');
    });

    panel.querySelector('.delete-team').addEventListener('click', function(){
      var typed = prompt('Delete ' + name + ' and every site it owns, for good? Type the team name to confirm:');
      if (typed === null) return;
      if (typed.trim() !== name) { alert('The name did not match, so nothing was deleted.'); return; }
      send('DELETE', teamPath(name) + '?confirm_name=' + encodeURIComponent(name)).then(function(res){
        if (!res.ok) { alert('Could not delete: ' + (res.body.error || 'unknown error')); return; }
        location.reload();
      }).catch(function(){ alert('Network error deleting the team.'); });
    });

    loadMembers();
  }

  if (createButton) {
    createButton.addEventListener('click', function(){
      var n = nameInput.value.trim();
      if (!n) return;
      createButton.disabled = true;
      send('POST', '/api/teams', {name: n}).then(function(res){
        createButton.disabled = false;
        if (!res.ok) { alert('Could not create the team: ' + (res.body.error || 'unknown error')); return; }
        location.reload();
      }).catch(function(){ createButton.disabled = false; alert('Network error creating the team.'); });
    });
  }

  load();
})();
</script>`
