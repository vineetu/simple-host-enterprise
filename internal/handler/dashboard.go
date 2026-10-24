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
	notificationsEnabled bool
	database             *sql.DB
	signingKeys          []auth.SigningKey
	sessionIdle          time.Duration
	quota                UploadQuota
	// hosts validates ?switch= (a site's "Switch account" link).
	hosts HostModel
}

func (h *DashboardHandler) WithNotifications(enabled bool) *DashboardHandler {
	h.notificationsEnabled = enabled
	return h
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
	mux.HandleFunc("GET /photos", h.photos)
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
  <span class="roadmap-text">Your usual username is reserved or already taken, so your account and site address use a longer version of it instead.</span>
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
  <p class="login-copy">A key authenticates CI or other automation as you. Mint one per job or machine so each can be revoked without touching the others. Keys expire after {{API_KEY_DEFAULT_DAYS}} days; mint a fresh one when yours does. A publish key can deploy, update and roll back your sites, read and write their saved data, and upload files; a full key can also delete sites and uploaded files, download a whole site, and change who can open it. No key can do administration, which needs a browser sign-in.</p>
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
  <h2 class="section-title">Your home page</h2>
  <p class="login-copy">Your person address opens your showcase, or a site you choose. Your address opens the site's usual URL; its access rules still apply. Its usual address keeps working; if it is unpublished, deleted, taken down or awaiting its certificate, your showcase appears. Teams keep their index.</p>
  <div class="add-row"><select id="home-site" aria-label="Home page"><option value="">Your showcase</option></select><button id="home-save" type="button" class="btn-login">Save home page</button></div>
  <p class="share-help">Your showcase feed is <code>/showcase.json</code> on your person address, behind company sign-in. It lists the same sites as your showcase for the current viewer.</p>
  <label for="showcase-bio">Your showcase bio</label><div class="add-row"><textarea id="showcase-bio" rows="3"></textarea><button id="bio-save" type="button" class="btn-login">Save bio</button></div><p id="home-status" role="status"></p>
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
</section>`))
	if h.notificationsEnabled {
		b.WriteString(notificationsPanel)
	}
	b.WriteString(`</main>`)
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
    pre.style.cssText = 'white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);color:var(--ink);padding:12px;border-radius:4px';
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

// dashboardSitesScript renders the signed-in person's sites as cards.
// Own and team sites (access_role owner or member) get Open and a Manage
// menu. The menu opens one section at a time, or acts at once for Download
// and Delete. Viewers stay in Shared with me. Placeholders such as
// {{DELETED_RETENTION}} are expanded when the page is served.
//
// Every fetch to a site-management endpoint carries
// X-Simple-Host-Client: control-ui, which is what exempts a browser
// (rather than the skill or MCP) from the skill-version guard
// (notice_middleware.go's isClassifiedNonSkillClient). Without it every
// one of these calls would 400 with "skill_version_required".
const dashboardSitesScript = `<script>
(function(){
  // Pure helpers stay above the DOM so a test run can call them and return.
  // Reach is only_me < specific < company < listed < network.
  var LEVELS = [
    ['only_me', 'Only me (or my team)'],
    ['specific', 'Specific people or teams'],
    ['company', 'Anyone in the company with the link'],
    ['listed', 'Listed in the showcase and search'],
    ['network', 'Anyone on the network, no sign-in (needs admin approval)']
  ];
  var SHORT_LEVEL = {only_me: 'only you', specific: 'named people', company: 'company', listed: 'listed', network: 'network'};
  var LEVEL_REACH = {only_me: 0, specific: 1, company: 2, listed: 3, network: 4};
  var SEARCH_MIN = 10;
  var SECTION_TITLE = {
    access: 'Who can open',
    versions: 'Versions',
    saved: 'Saved data',
    files: 'Uploaded files',
    activity: 'Activity and visitors',
    showcase: 'Your showcase',
    rename: 'Rename',
    move: 'Move to a team'
  };
  var CHEVRON = '<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>';

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
  function levelLabel(level) {
    for (var i = 0; i < LEVELS.length; i++) { if (LEVELS[i][0] === level) return LEVELS[i][1]; }
    return level || '';
  }
  function levelReach(level) {
    return Object.prototype.hasOwnProperty.call(LEVEL_REACH, level) ? LEVEL_REACH[level] : -1;
  }
  function widensAccess(from, to) {
    var a = levelReach(from), b = levelReach(to);
    return a >= 0 && b >= 0 && b > a;
  }
  function networkReasonRequired(level) { return level === 'network'; }
  function reasonAcceptable(text) { return String(text || '').trim() !== ''; }
  function addingViewerWidens(level) { return level === 'only_me'; }
  function accessState(site) {
    var label = SHORT_LEVEL[site && site.access] || '';
    var extra = '';
    if (site && site.access_decision && site.access_decision.decision === 'restricted') extra = 'restricted';
    else if (site && site.network_request) extra = 'requested';
    if (label && extra) return label + ', ' + extra;
    return extra || label;
  }
  // Showcase is the owner's own sites only, the same gate as "Your showcase".
  // Pass showcase as {pinned: true} once known. Omit the state while unknown.
  function menuItems(site, me, showcase) {
    site = site || {};
    var ver = Number(site.active_version) || 0;
    var items = [
      {id: 'access', label: 'Who can open', state: accessState(site), section: 'access'},
      {id: 'versions', label: 'Versions', state: ver > 0 ? ('v' + ver + ' live') : '', section: 'versions'},
      {id: 'saved', label: 'Saved data', section: 'saved'},
      {id: 'files', label: 'Uploaded files', section: 'files'},
      {id: 'activity', label: 'Activity and visitors', section: 'activity'}
    ];
    if (site.owner_username === me && site.access_role === 'owner') {
      items.push({id: 'showcase', label: 'Showcase', state: (showcase && showcase.pinned) ? 'pinned' : '', section: 'showcase'});
    }
    items.push({id: 'download', label: 'Download', act: 'download'});
    items.push({id: 'rename', label: 'Rename', section: 'rename'});
    items.push({id: 'move', label: 'Move to a team', section: 'move'});
    items.push({id: 'sep', sep: true});
    items.push({id: 'delete', label: 'Delete', act: 'delete', danger: true});
    return items;
  }
  function searchVisible(count) { return count >= SEARCH_MIN; }
  // Match the owner/name line and the team name (the owning namespace).
  function siteMatches(site, query) {
    var q = String(query || '').trim().toLowerCase();
    if (!q) return true;
    site = site || {};
    var owner = String(site.owner_username || '').toLowerCase();
    var name = String(site.name || '').toLowerCase();
    var team = String(site.team_name || '').toLowerCase();
    return (owner + '/' + name).indexOf(q) !== -1 || owner.indexOf(q) !== -1 || name.indexOf(q) !== -1 || (team !== '' && team.indexOf(q) !== -1);
  }
  // A delete here can be restored, so the dialog does not ask for the name.
  function deleteDialog(owner, name) {
    return {
      title: 'Delete "' + owner + '/' + name + '"?',
      body: 'It stops being served at once and moves to Recently deleted for {{DELETED_RETENTION}}, where it can be restored with its files, saved data, viewers, and uploads. After that it is gone for good.',
      confirmLabel: 'Delete site',
      danger: true,
      field: null
    };
  }
  function accessAsk(site, level) {
    var who = levelLabel(level);
    var spec = {
      title: 'Let more people open "' + site.owner_username + '/' + site.name + '"?',
      body: who + ' will be able to open it.',
      confirmLabel: 'Change who can open it'
    };
    if (networkReasonRequired(level)) {
      if (!widensAccess(site.access, level)) spec.title = 'Ask for network access for "' + site.owner_username + '/' + site.name + '"?';
      spec.body = who + ' will be able to open it. An admin must approve this.';
      spec.confirmLabel = 'Ask an admin';
      spec.field = {label: 'Why does it need to be open?', required: true};
    }
    return spec;
  }
  function viewerAsk(site) {
    return {
      title: 'Add viewers to "' + site.owner_username + '/' + site.name + '"?',
      body: 'Adding a viewer changes who can open it to specific people or teams.',
      confirmLabel: 'Add viewers'
    };
  }

  if (typeof __DASHBOARD_TEST__ !== 'undefined' && __DASHBOARD_TEST__) {
    var root = typeof globalThis !== 'undefined' ? globalThis : window;
    root.__dashboardSites = {
      menuItems: menuItems,
      widensAccess: widensAccess,
      levelReach: levelReach,
      accessState: accessState,
      searchVisible: searchVisible,
      siteMatches: siteMatches,
      reasonAcceptable: reasonAcceptable,
      networkReasonRequired: networkReasonRequired,
      addingViewerWidens: addingViewerWidens,
      deleteDialog: deleteDialog,
      accessAsk: accessAsk,
      viewerAsk: viewerAsk,
      levelLabel: levelLabel,
      SEARCH_MIN: SEARCH_MIN,
      LEVELS: LEVELS
    };
  }

  var container = document.getElementById('site-list');
  if (!container) return;
  var CH = {'X-Simple-Host-Client': 'control-ui'};
  var username = document.querySelector('.mast').getAttribute('data-username');
  var siteQuery = '';
  var searchOn = false;
  var showcaseCache = {};
  var rowMenuEl = null, rowMenuBtn = null;
  var dlg = null, dlgSettle = null;

  function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    var units = ['KB','MB','GB'], u = -1;
    do { n = n / 1024; u++; } while (n >= 1024 && u < units.length - 1);
    return n.toFixed(1) + ' ' + units[u];
  }
  function approvalProgress(req) {
    if (!req || !(req.approvals_required > 1)) return '';
    return ' (' + (req.approvals || 0) + ' of ' + req.approvals_required + ' approvals)';
  }
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
  function safeURL(u) { return /^https?:\/\//i.test(u || '') ? u : '#'; }
  function when(s) { var d = new Date(s); return isNaN(d) ? '' : d.toLocaleString(); }
  function liveLine(site) {
    var v = site.active_version > 0 ? 'version ' + site.active_version + ' live' : 'nothing published';
    return v + ' · updated ' + when(site.updated_at);
  }
  function siteBase(site) {
    return '/api/collaboration/sites/' + encodeURIComponent(site.owner_username) + '/' + encodeURIComponent(site.name);
  }
  function miItem(label, attrs, state, danger) {
    var aria = state ? ' aria-label="' + esc(label + ', ' + state) + '"' : '';
    return '<button type="button" class="mi' + (danger ? ' danger' : '') + '" role="menuitem"' + attrs + aria + '>' +
      esc(label) + (state ? '<span class="mi-state">' + esc(state) + '</span>' : '') + '</button>';
  }
  function menuMarkup(items) {
    return items.map(function(item) {
      if (item.sep) return '<div class="mi-sep" role="separator"></div>';
      var attrs = '';
      if (item.section) attrs += ' data-section="' + esc(item.section) + '"';
      if (item.act) attrs += ' data-act="' + esc(item.act) + '"';
      return miItem(item.label, attrs, item.state, item.danger);
    }).join('');
  }

  function ancestorBreaksFixed(el) {
    var n = el;
    while (n && n !== document.documentElement) {
      if (n.nodeType !== 1) { n = n.parentElement; continue; }
      var st = window.getComputedStyle(n);
      if ((st.transform && st.transform !== 'none') ||
          (st.filter && st.filter !== 'none') ||
          (st.contain && st.contain !== 'none')) return true;
      n = n.parentElement;
    }
    return false;
  }
  function closeRowMenu() {
    var btn = rowMenuBtn, el = rowMenuEl;
    if (el) { el.remove(); rowMenuEl = null; }
    if (btn) { btn.setAttribute('aria-expanded', 'false'); rowMenuBtn = null; }
    return btn;
  }
  function positionRowMenu(menu, btn) {
    var r = btn.getBoundingClientRect();
    var pad = 8;
    var mw = menu.offsetWidth, mh = menu.offsetHeight;
    var left = Math.max(pad, Math.min(window.innerWidth - mw - pad, r.right - mw));
    var below = r.bottom + 4, above = r.top - mh - 4, top;
    if (below + mh <= window.innerHeight - pad) top = below;
    else if (above >= pad) top = above;
    else top = pad;
    menu.style.left = left + 'px';
    menu.style.top = top + 'px';
  }
  function bindRowMenuKeys(menu) {
    menu.addEventListener('keydown', function(e) {
      var all = [].slice.call(menu.querySelectorAll('.mi:not([aria-disabled])'));
      if (!all.length) return;
      var i = all.indexOf(document.activeElement);
      if (i < 0) i = 0;
      if (e.key === 'ArrowDown') { e.preventDefault(); all[(i + 1) % all.length].focus(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); all[(i - 1 + all.length) % all.length].focus(); }
      else if (e.key === 'Home') { e.preventDefault(); all[0].focus(); }
      else if (e.key === 'End') { e.preventDefault(); all[all.length - 1].focus(); }
      else if (e.key === 'Escape') { e.preventDefault(); var b = closeRowMenu(); if (b) b.focus(); }
      else if (e.key === 'Tab') { closeRowMenu(); }
    });
  }
  function openRowMenu(btn, html, host, ariaLabel) {
    closeRowMenu();
    var menu = document.createElement('div');
    menu.className = 'row-menu';
    menu.setAttribute('role', 'menu');
    menu.setAttribute('aria-label', ariaLabel || 'Menu');
    menu.innerHTML = html;
    var parent = host;
    if (!parent || ancestorBreaksFixed(parent)) parent = document.body;
    if (parent !== document.body) {
      var after = btn.closest('.site-actions');
      if (after && after.parentNode === parent) parent.insertBefore(menu, after.nextSibling);
      else parent.appendChild(menu);
    } else {
      parent.appendChild(menu);
    }
    rowMenuEl = menu;
    rowMenuBtn = btn;
    btn.setAttribute('aria-expanded', 'true');
    positionRowMenu(menu, btn);
    bindRowMenuKeys(menu);
    var first = menu.querySelector('.mi:not([aria-disabled])');
    if (first) {
      try { first.focus({preventScroll: true}); }
      catch (err) { try { first.focus(); } catch (err2) {} }
    }
    menu.addEventListener('click', function(e) {
      var item = e.target.closest && e.target.closest('.mi');
      if (!item || item.getAttribute('aria-disabled') === 'true') return;
      e.stopPropagation();
      var trigger = rowMenuBtn;
      var section = item.getAttribute('data-section');
      var act = item.getAttribute('data-act');
      closeRowMenu();
      runAction(trigger, section, act);
    });
  }
  function openSiteMenu(btn) {
    var row = btn.closest('.site-card');
    var site = row && row._site;
    if (!site) return;
    var key = site.owner_username + '/' + site.name;
    openRowMenu(btn, menuMarkup(menuItems(site, username, showcaseCache[key])), row, 'Manage ' + site.owner_username + '/' + site.name);
    if (!(site.owner_username === username && site.access_role === 'owner') || showcaseCache[key]) return;
    fetch('/api/sites/' + encodeURIComponent(site.name) + '/showcase', {credentials: 'same-origin', headers: CH})
      .then(function(r) { return r.ok ? r.json() : null; })
      .then(function(d) {
        if (!d) return;
        showcaseCache[key] = {pinned: !!d.pinned};
        if (rowMenuBtn !== btn || !rowMenuEl || !d.pinned) return;
        var el = rowMenuEl.querySelector('[data-section="showcase"]');
        if (!el) return;
        var state = el.querySelector('.mi-state');
        if (!state) {
          state = document.createElement('span');
          state.className = 'mi-state';
          el.appendChild(state);
        }
        state.textContent = 'pinned';
        el.setAttribute('aria-label', 'Showcase, pinned');
      })
      .catch(function() {});
  }
  function runAction(btn, section, act) {
    var row = btn && btn.closest('.site-card');
    var site = row && row._site;
    if (!site) return;
    if (btn.focus) btn.focus();
    if (act === 'download') { downloadSite(site, btn); return; }
    if (act === 'delete') { deleteSite(site); return; }
    if (section) openSection(row, site, section);
  }

  function dialogFocusable(root) {
    var all = root.querySelectorAll('button, textarea, input, a[href]');
    var out = [], i;
    for (i = 0; i < all.length; i++) {
      var el = all[i];
      if (el.disabled || el.closest('[hidden]')) continue;
      out.push(el);
    }
    return out;
  }
  function finishDialog(ok) {
    if (!dlgSettle) return;
    var done = dlgSettle;
    dlgSettle = null;
    var area = dlg.querySelector('textarea');
    var value = area ? area.value : '';
    var kind = dlg.getAttribute('data-kind');
    if (dlg.open && dlg.close) dlg.close();
    else dlg.removeAttribute('open');
    var back = dlg._back;
    dlg._back = null;
    if (back && back.focus && document.contains(back)) { try { back.focus(); } catch (err) {} }
    if (kind === 'alert') done();
    else if (kind === 'field') done(ok ? value : null);
    else done(!!ok);
  }
  function ensureDialog() {
    if (dlg) return dlg;
    dlg = document.createElement('dialog');
    dlg.className = 'sh-dlg';
    dlg.setAttribute('role', 'dialog');
    dlg.setAttribute('aria-modal', 'true');
    dlg.setAttribute('aria-labelledby', 'sh-dlg-title');
    dlg.setAttribute('aria-describedby', 'sh-dlg-body');
    dlg.innerHTML =
      '<form class="sh-dlg-box" method="dialog" novalidate>' +
        '<h2 class="sh-dlg-title" id="sh-dlg-title"></h2>' +
        '<p class="sh-dlg-body" id="sh-dlg-body"></p>' +
        '<label class="sh-dlg-field"><span class="sh-dlg-label"></span><textarea class="sh-dlg-input" rows="3"></textarea></label>' +
        '<div class="sh-dlg-actions">' +
          '<button class="sh-dlg-btn" type="button" data-sh-dlg="cancel">Cancel</button>' +
          '<button class="sh-dlg-btn sh-dlg-primary" type="submit" data-sh-dlg="ok">OK</button>' +
        '</div>' +
      '</form>';
    document.body.appendChild(dlg);
    dlg.querySelector('form').addEventListener('submit', function(e) {
      e.preventDefault();
      if (dlg.querySelector('[data-sh-dlg="ok"]').disabled) return;
      finishDialog(true);
    });
    dlg.querySelector('[data-sh-dlg="cancel"]').addEventListener('click', function() { finishDialog(false); });
    dlg.addEventListener('cancel', function(e) { e.preventDefault(); finishDialog(false); });
    dlg.addEventListener('click', function(e) { if (e.target === dlg) finishDialog(false); });
    dlg.addEventListener('keydown', function(e) {
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finishDialog(false); return; }
      if (e.key !== 'Tab') return;
      var items = dialogFocusable(dlg);
      if (!items.length) return;
      var first = items[0], last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    });
    return dlg;
  }
  function ask(o) {
    o = o || {};
    ensureDialog();
    if (dlgSettle) finishDialog(false);
    var field = !!o.field;
    dlg.setAttribute('data-kind', o.alert ? 'alert' : (field ? 'field' : 'confirm'));
    var q = function(s) { return dlg.querySelector(s); };
    q('.sh-dlg-title').textContent = o.title || '';
    var body = q('.sh-dlg-body');
    body.textContent = o.body || '';
    body.hidden = !o.body;
    var fieldWrap = q('.sh-dlg-field');
    var area = q('textarea');
    fieldWrap.hidden = !field;
    q('.sh-dlg-label').textContent = field && o.field.label ? o.field.label : '';
    area.value = field && o.field.value ? o.field.value : '';
    var okBtn = q('[data-sh-dlg="ok"]');
    var cancelBtn = q('[data-sh-dlg="cancel"]');
    okBtn.textContent = o.confirmLabel || 'OK';
    okBtn.classList.toggle('sh-dlg-danger', !!o.danger);
    cancelBtn.textContent = o.cancelLabel || 'Cancel';
    cancelBtn.hidden = !!o.alert;
    var required = field && o.field.required;
    area.oninput = function() { okBtn.disabled = required && !reasonAcceptable(area.value); };
    okBtn.disabled = required && !reasonAcceptable(area.value);
    var back = document.activeElement;
    return new Promise(function(resolve) {
      dlg._back = back;
      dlgSettle = function(result) { resolve(result); };
      if (!dlg.open) { if (dlg.showModal) dlg.showModal(); else dlg.setAttribute('open', ''); }
      var focusEl = field ? area : (o.danger && !o.alert ? cancelBtn : okBtn);
      if (focusEl && focusEl.focus) focusEl.focus();
    });
  }
  function tell(message) { return ask({title: message, confirmLabel: 'OK', alert: true}); }
  function accepted(result) { return result !== false && result != null; }

  function loadSites() {
    fetch('/api/collaboration/sites?include=shared', {credentials: 'same-origin', headers: CH})
      .then(function(r) { return r.json(); })
      .then(function(all) {
        all = all || [];
        renderShared(all.filter(function(s) { return s.access_role === 'viewer'; }));
        renderSites(all.filter(function(s) { return s.access_role !== 'viewer'; }));
      })
      .catch(function() {
        closeRowMenu();
        container.innerHTML = '<div class="rank-empty">Could not load sites.</div>';
        var line = document.getElementById('site-search-line');
        if (line) line.hidden = true;
        var empty = document.getElementById('site-nomatch');
        if (empty) empty.hidden = true;
        searchOn = false;
      });
  }

  function renderFirstRun() {
    container.innerHTML = '';
    var box = document.createElement('div');
    box.className = 'first-run';
    var lead = document.createElement('p');
    lead.className = 'login-copy';
    lead.textContent = 'No sites yet. Your sites come from your AI app: add this server to ChatGPT, Claude, Copilot, Cursor or Codex, sign in with your work account when it asks, then ask it to publish something.';
    var pre = document.createElement('pre');
    pre.className = 'mcp-address';
    pre.style.cssText = 'white-space:pre-wrap;word-break:break-all;background:var(--ps-blue-50);color:var(--ink);padding:12px;border-radius:4px';
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

  function renderShared(sites) {
    var section = document.getElementById('shared-section');
    var list = document.getElementById('shared-list');
    if (!section || !list) return;
    section.hidden = !sites.length;
    list.innerHTML = sites.map(function(site) {
      var via = site.shared_via ? 'shared with ' + site.shared_via : 'shared with you';
      return '<div class="rank-row"><span class="rank-name"><a href="' + esc(safeURL(site.url)) + '">' + esc(site.owner_username) + '/' + esc(site.name) + '</a>' +
        ' <span class="rank-sub">' + esc(via) + ' · updated ' + esc(when(site.updated_at)) + '</span></span></div>';
    }).join('');
  }

  function ensureSearch() {
    var line = document.getElementById('site-search-line');
    if (line) return line;
    line = document.createElement('div');
    line.id = 'site-search-line';
    line.className = 'site-search-line';
    line.hidden = true;
    line.innerHTML = '<label for="site-search">Search your sites</label><input id="site-search" type="search" autocomplete="off">';
    var nomatch = document.createElement('div');
    nomatch.id = 'site-nomatch';
    nomatch.className = 'rank-empty site-nomatch';
    nomatch.hidden = true;
    nomatch.textContent = 'No sites match.';
    container.parentNode.insertBefore(line, container);
    container.parentNode.insertBefore(nomatch, container);
    line.querySelector('input').addEventListener('input', function() {
      siteQuery = this.value;
      applySiteSearch();
    });
    return line;
  }
  function setSearch(count) {
    var line = ensureSearch();
    searchOn = searchVisible(count);
    line.hidden = !searchOn;
    if (!searchOn) siteQuery = '';
    var input = line.querySelector('input');
    if (input && input.value !== siteQuery) input.value = siteQuery;
    if (!searchOn) {
      var empty = document.getElementById('site-nomatch');
      if (empty) empty.hidden = true;
    }
  }
  function applySiteSearch() {
    var rows = container.querySelectorAll('.site-card');
    var q = searchOn ? siteQuery : '';
    var shown = 0;
    Array.prototype.forEach.call(rows, function(row) {
      var match = siteMatches(row._site, q);
      if (!match) {
        if (rowMenuBtn && row.contains(rowMenuBtn)) closeRowMenu();
        var panel = row.querySelector('.site-panel');
        if (panel && !panel.hidden) closeSection(row, false);
      }
      row.hidden = !match;
      if (match) shown++;
    });
    var empty = document.getElementById('site-nomatch');
    if (empty) empty.hidden = !(String(q || '').trim() && rows.length && shown === 0);
  }

  function renderSites(sites) {
    var select = document.getElementById('home-site');
    select.innerHTML = '<option value="">Your showcase</option>';
    (sites || []).filter(function(s) { return s.owner_username === username && s.access_role === 'owner'; }).forEach(function(s) {
      var o = document.createElement('option');
      o.value = s.name;
      o.textContent = s.name;
      select.appendChild(o);
    });
    fetch('/api/me/home', {headers: CH}).then(function(r) { return r.json(); }).then(function(d) { select.value = d.site || ''; });
    closeRowMenu();
    if (!sites || !sites.length) { setSearch(0); renderFirstRun(); return; }
    setSearch(sites.length);
    container.innerHTML = '';
    sites.forEach(function(site) {
      var canManage = site.access_role === 'owner' || site.access_role === 'member';
      var row = document.createElement('div');
      row.className = 'rank-row site-row site-card';
      row._site = site;
      var label = site.owner_username + '/' + site.name;
      row.innerHTML = '<div class="site-id"><span class="rank-name"><a href="' + esc(safeURL(site.url)) + '">' + esc(label) + '</a>' +
        ' <span class="rank-sub">' + esc(site.access_role) + ' · ' + esc(levelLabel(site.access)) +
        (site.network_request ? ' · network access requested' + approvalProgress(site.network_request) : esc(decisionShort(site))) +
        ' · ' + esc(liveLine(site)) + '</span></span></div>' +
        '<div class="site-actions"><a class="btn-login site-open" href="' + esc(safeURL(site.url)) + '" target="_blank" rel="noopener">Open</a>' +
        (canManage ? '<button type="button" class="btn-login site-manage" aria-haspopup="menu" aria-expanded="false" aria-label="Manage ' + esc(label) + '">Manage ' + CHEVRON + '</button>' : '') +
        '</div>';
      var panel = document.createElement('div');
      panel.className = 'site-panel';
      panel.hidden = true;
      row.appendChild(panel);
      container.appendChild(row);
      if (!canManage) return;
      panel.addEventListener('keydown', function(e) {
        if (e.key !== 'Escape') return;
        e.preventDefault();
        e.stopPropagation();
        closeSection(row, true);
      });
    });
    applySiteSearch();
  }

  function closeSection(row, restoreFocus) {
    var panel = row.querySelector('.site-panel');
    if (!panel) return;
    panel.hidden = true;
    panel.innerHTML = '';
    panel.removeAttribute('data-section');
    if (restoreFocus === false) return;
    var manage = row.querySelector('.site-manage');
    if (manage && manage.focus) manage.focus();
  }
  function openSection(row, site, section) {
    var panel = row.querySelector('.site-panel');
    if (!panel) return;
    if (!panel.hidden && panel.getAttribute('data-section') === section) {
      closeSection(row, true);
      return;
    }
    panel.hidden = false;
    panel.setAttribute('data-section', section);
    renderSection(panel, site, section);
    if (panel.scrollIntoView) panel.scrollIntoView({block: 'nearest'});
  }
  function focusSection(panel) {
    var nodes = panel.querySelectorAll('select, input, textarea, button, a');
    var i;
    for (i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      if (el.disabled || el.hidden || el.classList.contains('site-panel-close')) continue;
      el.focus();
      return;
    }
    var closeBtn = panel.querySelector('.site-panel-close');
    if (closeBtn) closeBtn.focus();
  }
  function sectionBody(site, section) {
    var locked = site.access_decision && site.access_decision.decision === 'restricted' ? ' disabled' : '';
    var options = LEVELS.map(function(l) {
      return '<option value="' + l[0] + '"' + (l[0] === site.access ? ' selected' : '') + '>' + esc(l[1]) + '</option>';
    }).join('');
    if (section === 'access') {
      return '<div class="site-subsection"><div class="add-row"><select class="access-select"' + locked + '>' + options + '</select>' +
        '<button type="button" class="btn-login access-button"' + locked + '>Save</button></div>' +
        '<p class="share-help access-status">' + (site.network_request ? 'Network access requested; waiting for ' + (site.network_request.approvals_required > 1 ? 'two admins' + approvalProgress(site.network_request) : 'an admin') + '. The site keeps its current level until then.' : esc(decisionNote(site))) + '</p></div>' +
        '<div class="site-subsection"><h4>Viewers</h4>' +
        '<p class="share-help">Named viewers can open the site while it is set to specific people or teams. Adding one sets that level. Someone who hasn\'t signed in yet can be added by work email; they can open the site after their first sign-in.</p>' +
        '<div class="viewer-list" aria-live="polite"></div>' +
        '<div class="add-row"><input type="text" class="add-viewer-input" placeholder="username or work email, another" autocomplete="off"' + locked + '>' +
        '<button type="button" class="btn-login add-viewer-button"' + locked + '>Add</button></div></div>';
    }
    if (section === 'versions') {
      return '<div class="site-subsection"><p class="share-help">Preview opens a version in a new tab, for you and your team only, for {{PREVIEW_LINK_TTL}}. Make live shows it to visitors at once; the other versions stay here.</p>' +
        '<div class="version-list" aria-live="polite"></div></div>';
    }
    if (section === 'saved') {
      return '<div class="site-subsection"><p class="share-help">The last versions of what the site\'s pages have saved. Restore puts an earlier one back as the current saved data; the data it replaces stays in this list.</p>' +
        '<div class="state-list" aria-live="polite"></div></div>';
    }
    if (section === 'files') {
      return '<div class="site-subsection"><div class="asset-list" aria-live="polite"></div></div>';
    }
    if (section === 'activity') {
      return '<div class="site-subsection site-tabs"><div class="site-tab-buttons">' +
        '<button type="button" class="btn-reject site-tab-button active" data-tab="activity">Activity</button>' +
        '<button type="button" class="btn-reject site-tab-button" data-tab="visitors">Visitors</button>' +
        '</div>' +
        '<div class="site-tab-panel activity-tab"><div class="activity-list" aria-live="polite"></div></div>' +
        '<div class="site-tab-panel visitor-tab" hidden><div class="visitor-list" aria-live="polite"></div></div>' +
        '</div>';
    }
    if (section === 'showcase') {
      return '<div class="site-subsection"><label><input type="checkbox" class="showcase-pin"> Pinned</label> <label>Order <input type="number" class="showcase-order" min="0" max="1000000" value="0"></label> <button type="button" class="btn-login showcase-save">Save</button><p class="share-help showcase-status">Pin first, then smaller order numbers. Access stays unchanged.</p></div>';
    }
    if (section === 'rename') {
      return '<div class="site-subsection"><p class="share-help">The site gets a new address. The old one sends visitors on to it until another site takes the name.</p>' +
        '<div class="add-row"><input type="text" class="rename-input" placeholder="new-name" autocomplete="off"' + locked + '>' +
        '<button type="button" class="btn-login rename-button"' + locked + '>Rename</button></div></div>';
    }
    if (section === 'move') {
      return '<div class="site-subsection"><p class="share-help">Move the site into a team you are in. Its files, versions, saved data, uploads, access and viewers go with it, and the old address sends visitors on to the new one. Network access has to be requested again.</p>' +
        '<div class="add-row"><input type="text" class="transfer-input" placeholder="team-name" autocomplete="off"' + locked + '>' +
        '<button type="button" class="btn-login transfer-button"' + locked + '>Move</button></div></div>';
    }
    return '';
  }
  function renderSection(panel, site, section) {
    var title = SECTION_TITLE[section] || 'Manage';
    panel.innerHTML = '<div class="site-panel-bar"><h4>' + esc(title) + '</h4><button type="button" class="btn-reject site-panel-close">Close</button></div>' + sectionBody(site, section);
    panel.querySelector('.site-panel-close').addEventListener('click', function() {
      closeSection(panel.closest('.site-card'), true);
    });
    wireSection(panel, site, section);
    focusSection(panel);
  }

  function wireSection(panel, site, section) {
    var owner = site.owner_username, name = site.name;
    var base = siteBase(site);
    var viewerList = panel.querySelector('.viewer-list');
    var assetList = panel.querySelector('.asset-list');
    var activityList = panel.querySelector('.activity-list');
    var visitorList = panel.querySelector('.visitor-list');
    var versionList = panel.querySelector('.version-list');
    var stateList = panel.querySelector('.state-list');
    var auditQuery = '/api/audit?owner=' + encodeURIComponent(owner) + '&site=' + encodeURIComponent(name);
    var accessQuery = '/api/access?owner=' + encodeURIComponent(owner) + '&site=' + encodeURIComponent(name);

    if (section === 'showcase') {
      var c = panel.querySelector('.site-subsection');
      var key = owner + '/' + name;
      fetch('/api/sites/' + encodeURIComponent(name) + '/showcase', {headers: CH}).then(function(r) { return r.json(); }).then(function(d) {
        c.querySelector('.showcase-pin').checked = d.pinned;
        c.querySelector('.showcase-order').value = d.order;
        showcaseCache[key] = {pinned: !!d.pinned};
      });
      c.querySelector('.showcase-save').onclick = function() {
        savePresentation('/api/sites/' + encodeURIComponent(name) + '/showcase', {pinned: c.querySelector('.showcase-pin').checked, order: Number(c.querySelector('.showcase-order').value)}).then(function() {
          showcaseCache[key] = {pinned: c.querySelector('.showcase-pin').checked};
          c.querySelector('.showcase-status').textContent = 'Showcase saved.';
        }).catch(function(e) { c.querySelector('.showcase-status').textContent = e.message; });
      };
    }

    panel.querySelectorAll('.site-tab-button').forEach(function(button) {
      button.addEventListener('click', function() {
        panel.querySelectorAll('.site-tab-button').forEach(function(b) { b.classList.remove('active'); });
        button.classList.add('active');
        var showActivity = button.getAttribute('data-tab') === 'activity';
        panel.querySelector('.activity-tab').hidden = !showActivity;
        panel.querySelector('.visitor-tab').hidden = showActivity;
      });
    });

    function loadActivity() {
      fetch(auditQuery, {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(body) {
          var events = (body && body.events) || [];
          activityList.innerHTML = '';
          if (!events.length) { activityList.innerHTML = '<div class="rank-empty">No recorded actions yet.</div>'; return; }
          events.forEach(function(e) {
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(e.action) +
              ' <span class="rank-sub">' + (e.actor_name ? esc(e.actor_name) + ' · ' : '') + esc(new Date(e.at).toLocaleString()) + '</span></span>';
            activityList.appendChild(row);
          });
        })
        .catch(function() { activityList.innerHTML = '<div class="rank-empty">Could not load activity.</div>'; });
    }
    function appendTopLists(target, body) {
      [['Top pages', body.top_pages, 'path'], ['Where visitors came from', body.top_referrers, 'domain']].forEach(function(list) {
        if (!list[1] || !list[1].length) return;
        var head = document.createElement('div');
        head.className = 'rank-row';
        head.innerHTML = '<span class="rank-name"><strong>' + esc(list[0]) + '</strong> <span class="rank-sub">last 30 days, people only</span></span>';
        target.appendChild(head);
        list[1].forEach(function(t) {
          var row = document.createElement('div');
          row.className = 'rank-row';
          row.innerHTML = '<span class="rank-name">' + esc(t[list[2]]) + ' <span class="rank-sub">' + esc(t.views) + ' views</span></span>';
          target.appendChild(row);
        });
      });
    }
    function loadVisitors() {
      fetch(accessQuery, {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(body) {
          visitorList.innerHTML = '';
          if (body && body.days) {
            if (!body.days.length) { visitorList.innerHTML = '<div class="rank-empty">No recorded visits in the last 30 days.</div>'; return; }
            var total = document.createElement('div');
            total.className = 'rank-row';
            total.innerHTML = '<span class="rank-name">' + esc(body.unique_viewers) + ' people viewed this site in the last 30 days</span>';
            visitorList.appendChild(total);
            appendTopLists(visitorList, body);
            var daysHead = document.createElement('div');
            daysHead.className = 'rank-row';
            daysHead.innerHTML = '<span class="rank-name"><strong>By day</strong></span>';
            visitorList.appendChild(daysHead);
            body.days.forEach(function(d) {
              var row = document.createElement('div');
              row.className = 'rank-row';
              row.innerHTML = '<span class="rank-name">' + esc(d.day) + ' <span class="rank-sub">' + esc(d.views) + ' views · ' + esc(d.unique_viewers) + ' people</span></span>';
              visitorList.appendChild(row);
            });
            return;
          }
          var entries = (body && body.entries) || [];
          if (!entries.length) { visitorList.innerHTML = '<div class="rank-empty">No recorded visits yet.</div>'; return; }
          var tops = document.createElement('div');
          visitorList.appendChild(tops);
          fetch(accessQuery + '&summary=counts', {credentials: 'same-origin', headers: CH})
            .then(function(r) { return r.ok ? r.json() : null; })
            .then(function(counts) { if (counts) appendTopLists(tops, counts); })
            .catch(function() {});
          entries.forEach(function(e) {
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(e.method) + ' ' + esc(e.path) +
              ' <span class="rank-sub">' + esc(e.status) + ' · ' + esc(e.client_kind) + (e.referrer_domain ? ' · from ' + esc(e.referrer_domain) : '') + ' · ' + esc(new Date(e.at).toLocaleString()) + '</span></span>';
            visitorList.appendChild(row);
          });
        })
        .catch(function() { visitorList.innerHTML = '<div class="rank-empty">Could not load visitors.</div>'; });
    }
    function loadViewers() {
      fetch(base + '/viewers', {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(viewers) {
          viewerList.innerHTML = '';
          if (!viewers || !viewers.length) { viewerList.innerHTML = '<div class="rank-empty">No named viewers.</div>'; return; }
          viewers.forEach(function(v) {
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">' + esc(v.username) + ' <span class="rank-sub">' + (v.pending ? 'hasn\'t signed in yet' : esc(v.kind)) + '</span></span>' +
              '<button type="button" class="btn-reject remove-viewer" data-username="' + esc(v.username) + '">Remove</button>';
            viewerList.appendChild(row);
          });
        })
        .catch(function() { viewerList.innerHTML = '<div class="rank-empty">Could not load viewers.</div>'; });
    }
    function loadAssets() {
      fetch(base + '/assets', {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(assets) {
          assetList.innerHTML = '';
          if (!assets || !assets.length) { assetList.innerHTML = '<div class="rank-empty">No uploaded files.</div>'; return; }
          assets.forEach(function(a) {
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name"><a href="' + esc(a.url) + '">' + esc(a.name) + '</a> <span class="rank-sub">' + esc(a.content_type) + ' · ' + fmtBytes(a.size) + '</span></span>' +
              '<button type="button" class="btn-reject remove-asset" data-id="' + esc(a.id) + '">Delete</button>';
            assetList.appendChild(row);
          });
        })
        .catch(function() { assetList.innerHTML = '<div class="rank-empty">Could not load assets.</div>'; });
    }
    function saveAccess(level, reason) {
      var payload = {level: level};
      if (reason) payload.reason = String(reason).trim();
      fetch(base + '/access', {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json'}, CH),
        body: JSON.stringify(payload),
      }).then(function(r) {
        return r.json().then(function(b) {
          if (!r.ok) { tell('Could not change access: ' + (b.error || 'unknown error')); return; }
          var status = panel.querySelector('.access-status');
          if (status) status.textContent = b.note || '';
          site.access = level;
          loadSites();
        });
      }).catch(function() { tell('Network error changing access.'); });
    }

    var accessButton = panel.querySelector('.access-button');
    if (accessButton) accessButton.addEventListener('click', function() {
      var level = panel.querySelector('.access-select').value;
      if (!(widensAccess(site.access, level) || networkReasonRequired(level))) { saveAccess(level, ''); return; }
      var spec = accessAsk(site, level);
      ask(spec).then(function(result) {
        if (!accepted(result)) return;
        var reason = spec.field ? String(result) : '';
        if (spec.field && !reasonAcceptable(reason)) return;
        saveAccess(level, reason);
      });
    });

    var addViewer = panel.querySelector('.add-viewer-button');
    if (addViewer) addViewer.addEventListener('click', function() {
      var input = panel.querySelector('.add-viewer-input');
      var usernames = input.value.split(',').map(function(s) { return s.trim(); }).filter(Boolean);
      if (!usernames.length) return;
      var send = function() {
        fetch(base + '/viewers', {
          method: 'POST', credentials: 'same-origin',
          headers: Object.assign({'Content-Type': 'application/json'}, CH),
          body: JSON.stringify({usernames: usernames}),
        }).then(function(r) {
          if (!r.ok) return r.json().then(function(b) { tell('Could not add: ' + (b.error || 'unknown error')); });
          input.value = '';
          if (addingViewerWidens(site.access)) {
            site.access = 'specific';
            var sel = panel.querySelector('.access-select');
            if (sel) sel.value = 'specific';
          }
          loadViewers();
        }).catch(function() { tell('Network error adding viewers.'); });
      };
      if (addingViewerWidens(site.access)) ask(viewerAsk(site)).then(function(ok) { if (ok) send(); });
      else send();
    });

    function moveSite(path, payload, spec) {
      ask(spec).then(function(ok) {
        if (!ok) return;
        fetch(base + path, {
          method: 'POST', credentials: 'same-origin',
          headers: Object.assign({'Content-Type': 'application/json'}, CH),
          body: JSON.stringify(payload),
        }).then(function(r) {
          return r.json().then(function(b) {
            if (!r.ok) { tell('Could not move the site: ' + (b.error || 'unknown error')); return; }
            ask({
              title: b.owner + '/' + b.name + ' is now at ' + b.url,
              body: 'The old address sends visitors there.',
              confirmLabel: 'OK',
              alert: true
            }).then(function() { loadSites(); });
          });
        }).catch(function() { tell('Network error moving the site.'); });
      });
    }
    var renameButton = panel.querySelector('.rename-button');
    if (renameButton) renameButton.addEventListener('click', function() {
      var next = panel.querySelector('.rename-input').value.trim();
      if (!next) return;
      moveSite('/rename', {name: next}, {
        title: 'Rename ' + site.name + ' to ' + next + '?',
        body: 'Its address changes; the old one redirects.',
        confirmLabel: 'Rename'
      });
    });
    var transferButton = panel.querySelector('.transfer-button');
    if (transferButton) transferButton.addEventListener('click', function() {
      var to = panel.querySelector('.transfer-input').value.trim();
      if (!to) return;
      moveSite('/transfer', {to: to}, {
        title: 'Move ' + owner + '/' + site.name + ' into ' + to + '?',
        body: 'Every member of that team can then change or delete it.',
        confirmLabel: 'Move'
      });
    });

    if (viewerList) viewerList.addEventListener('click', function(ev) {
      var button = ev.target.closest('.remove-viewer');
      if (!button) return;
      fetch(base + '/viewers/' + encodeURIComponent(button.getAttribute('data-username')), {method: 'DELETE', credentials: 'same-origin', headers: CH})
        .then(loadViewers)
        .catch(function() { tell('Network error removing viewer.'); });
    });
    if (assetList) assetList.addEventListener('click', function(ev) {
      var button = ev.target.closest('.remove-asset');
      if (!button) return;
      ask({
        title: 'Delete this asset?',
        body: 'Any page still linking to it will break.',
        confirmLabel: 'Delete',
        danger: true
      }).then(function(ok) {
        if (!ok) return;
        fetch(base + '/assets/' + encodeURIComponent(button.getAttribute('data-id')), {method: 'DELETE', credentials: 'same-origin', headers: CH})
          .then(loadAssets)
          .catch(function() { tell('Network error deleting asset.'); });
      });
    });

    function loadVersions() {
      fetch(base + '/versions', {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(versions) {
          versionList.innerHTML = '';
          if (!versions || !versions.length) { versionList.innerHTML = '<div class="rank-empty">No versions yet.</div>'; return; }
          versions.forEach(function(v) {
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
        .catch(function() { versionList.innerHTML = '<div class="rank-empty">Could not load versions.</div>'; });
    }
    function loadStateVersions() {
      fetch(base + '/state-versions', {credentials: 'same-origin', headers: CH})
        .then(function(r) { return r.json(); })
        .then(function(entries) {
          stateList.innerHTML = '';
          if (!entries || !entries.length) { stateList.innerHTML = '<div class="rank-empty">Nothing saved yet.</div>'; return; }
          entries.forEach(function(e, i) {
            var row = document.createElement('div');
            row.className = 'rank-row';
            row.innerHTML = '<span class="rank-name">Saved data version ' + esc(e.version) +
              ' <span class="rank-sub">' + esc(when(e.created_at)) + (e.written_by ? ' · by ' + esc(e.written_by) : '') + ' · ' + fmtBytes(e.bytes || 0) + '</span></span>' +
              (i === 0 ? '<span class="rank-metric">current</span>' :
                '<button type="button" class="btn-login restore-state" data-id="' + esc(e.id) + '" data-version="' + esc(e.version) + '">Restore</button>');
            stateList.appendChild(row);
          });
        })
        .catch(function() { stateList.innerHTML = '<div class="rank-empty">Could not load saved data history.</div>'; });
    }
    if (versionList) versionList.addEventListener('click', function(ev) {
      var preview = ev.target.closest('.preview-version');
      if (preview) {
        var win = window.open('', '_blank');
        fetch(base + '/versions/' + encodeURIComponent(preview.getAttribute('data-version')) + '/preview', {credentials: 'same-origin', headers: CH})
          .then(function(r) { return r.json().then(function(b) {
            if (!r.ok) { if (win) win.close(); tell('Could not open the preview: ' + (b.error || 'unknown error')); return; }
            if (win) { win.opener = null; win.location = b.url; } else { location.href = b.url; }
          }); })
          .catch(function() { if (win) win.close(); tell('Network error opening the preview.'); });
        return;
      }
      var button = ev.target.closest('.make-live');
      if (!button) return;
      var version = parseInt(button.getAttribute('data-version'), 10);
      ask({
        title: 'Make version ' + version + ' of ' + site.name + ' live?',
        body: 'Visitors see it at once.',
        confirmLabel: 'Make live'
      }).then(function(ok) {
        if (!ok) return;
        button.disabled = true;
        fetch(base + '/rollback', {
          method: 'POST', credentials: 'same-origin',
          headers: Object.assign({'Content-Type': 'application/json', 'If-Match': site.etag}, CH),
          body: JSON.stringify({version: version}),
        }).then(function(r) {
          return r.json().then(function(b) {
            if (!r.ok) {
              button.disabled = false;
              if (button.focus) button.focus();
              tell(r.status === 412 ? 'The site changed since this page loaded. Reload and try again.' : 'Could not make it live: ' + (b.error || 'unknown error'));
              return;
            }
            site.active_version = b.active_version;
            site.etag = b.etag;
            loadVersions();
            loadSites();
          });
        }).catch(function() { button.disabled = false; if (button.focus) button.focus(); tell('Network error changing the live version.'); });
      });
    });
    if (stateList) stateList.addEventListener('click', function(ev) {
      var button = ev.target.closest('.restore-state');
      if (!button) return;
      ask({
        title: 'Restore saved data version ' + button.getAttribute('data-version') + '?',
        body: 'It replaces what the pages have saved now; the data it replaces stays in this list.',
        confirmLabel: 'Restore'
      }).then(function(ok) {
        if (!ok) return;
        button.disabled = true;
        fetch(base + '/state-versions/' + encodeURIComponent(button.getAttribute('data-id')) + '/restore', {method: 'POST', credentials: 'same-origin', headers: CH})
          .then(function(r) {
            return r.json().then(function(b) {
              if (!r.ok) { button.disabled = false; if (button.focus) button.focus(); tell('Could not restore: ' + (b.error || 'unknown error')); return; }
              loadStateVersions();
            });
          })
          .catch(function() { button.disabled = false; if (button.focus) button.focus(); tell('Network error restoring saved data.'); });
      });
    });

    if (activityList) loadActivity();
    if (visitorList) loadVisitors();
    if (viewerList) loadViewers();
    if (assetList) loadAssets();
    if (versionList) loadVersions();
    if (stateList) loadStateVersions();
  }

  function downloadSite(site, button) {
    if (button) button.disabled = true;
    fetch(siteBase(site) + '/export-link', {method: 'POST', credentials: 'same-origin', headers: CH})
      .then(function(r) {
        return r.json().then(function(b) {
          if (button) button.disabled = false;
          if (!r.ok || !b.url) {
            if (button && button.focus) button.focus();
            tell('Could not prepare the download: ' + (b.error || 'unknown error'));
            return;
          }
          location.href = b.url;
        });
      })
      .catch(function() {
        if (button) { button.disabled = false; if (button.focus) button.focus(); }
        tell('Network error preparing the download.');
      });
  }
  function deleteSite(site) {
    ask(deleteDialog(site.owner_username, site.name)).then(function(ok) {
      if (!ok) return;
      fetch(siteBase(site), {method: 'DELETE', credentials: 'same-origin', headers: CH})
        .then(function(r) {
          if (r.ok) { location.reload(); return; }
          return r.json().then(function(b) { tell('Could not delete: ' + (b.error || 'unknown error')); });
        })
        .catch(function() { tell('Network error deleting the site.'); });
    });
  }

  document.addEventListener('click', function(e) {
    var trigger = e.target.closest && e.target.closest('.site-manage');
    if (trigger) {
      if (rowMenuBtn === trigger) closeRowMenu();
      else openSiteMenu(trigger);
      return;
    }
    if (rowMenuEl && !(e.target.closest && e.target.closest('.row-menu'))) closeRowMenu();
  });
  document.addEventListener('keydown', function(e) {
    var t = e.target.closest && e.target.closest('.site-manage');
    if (!t) return;
    if (e.key === 'ArrowDown' && rowMenuBtn !== t) {
      e.preventDefault();
      openSiteMenu(t);
    }
  });
  window.addEventListener('resize', function() { closeRowMenu(); });
  window.addEventListener('orientationchange', function() { closeRowMenu(); });
  // A page scroll, including the one a browser makes to bring the button
  // into view as the menu opens, keeps the menu on its button. It closes
  // only once the button has left the screen. A scroll inside the menu does not.
  window.addEventListener('scroll', function(e) {
    if (!rowMenuEl || !rowMenuBtn) return;
    if (e.target === rowMenuEl || (rowMenuEl.contains && rowMenuEl.contains(e.target))) return;
    var r = rowMenuBtn.getBoundingClientRect();
    if (r.bottom < 0 || r.top > window.innerHeight) { closeRowMenu(); return; }
    positionRowMenu(rowMenuEl, rowMenuBtn);
  }, true);

  function savePresentation(path, body) {
    return fetch(path, {method: 'PUT', headers: {'Content-Type': 'application/json', 'X-Simple-Host-Client': 'control-ui'}, body: JSON.stringify(body)}).then(function(r) {
      return r.json().then(function(d) { if (!r.ok) throw new Error(d.error || 'Save failed'); return d; });
    });
  }
  document.getElementById('home-save').onclick = function() {
    savePresentation('/api/me/home', {site: document.getElementById('home-site').value || null}).then(function() {
      document.getElementById('home-status').textContent = 'Home page saved.';
    }).catch(function(e) { document.getElementById('home-status').textContent = e.message; });
  };
  document.getElementById('bio-save').onclick = function() {
    savePresentation('/api/me/bio', {bio: document.getElementById('showcase-bio').value}).then(function() {
      document.getElementById('home-status').textContent = 'Bio saved.';
    }).catch(function(e) { document.getElementById('home-status').textContent = e.message; });
  };
  fetch('/api/me/bio', {headers: CH}).then(function(r) { return r.json(); }).then(function(d) {
    document.getElementById('showcase-bio').value = d.bio || '';
    document.getElementById('showcase-bio').maxLength = d.max_length;
  });
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
