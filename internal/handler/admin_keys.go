package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
)

// revokeLeakedKeyRequest carries the key an admin found somewhere it should
// not be. It is hashed and looked up, never stored, logged or echoed back.
type revokeLeakedKeyRequest struct {
	Key string `json:"key"`
}

type revokedKeyResponse struct {
	Status    string `json:"status"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	KeyID     string `json:"key_id"`
	Label     string `json:"label"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// revokeLeakedKey answers POST /api/admin/keys/revoke: an admin pastes a
// key found in a gist, a log or a commit, and whichever person's key it is
// is revoked on its own, without disabling that person. Audited as
// admin_key_revoke with the owner's name and the key's id and name. A key
// that is already revoked answers 200 "already revoked" with its owner, so
// the admin knows the leak is already closed; one that matches nothing
// answers 404.
func (h *AdminHandler) revokeLeakedKey(w http.ResponseWriter, r *http.Request) {
	var req revokeLeakedKeyRequest
	if !decodeSmallJSON(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "paste the key to revoke"})
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("admin: revoke leaked key: begin: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	rec, err := db.GetAPIKeyByHash(r.Context(), tx, db.HashAPIKey(key))
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no API key matches that; check it was copied whole"})
		return
	}
	if err != nil {
		log.Printf("admin: revoke leaked key: look up: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := revokedKeyResponse{Owner: rec.Owner, Name: rec.Name, KeyID: rec.ID, Label: keyLabel(rec.Last4)}
	if rec.RevokedAt != nil {
		out.Status = "already revoked"
		out.RevokedAt = rec.RevokedAt.UTC().Format(time.RFC3339)
		writeJSON(w, http.StatusOK, out)
		return
	}
	err = db.RevokeAPIKeyByID(r.Context(), tx, rec.ID)
	if err == nil {
		err = h.audit.RecordTx(r.Context(), tx, h.userAuditEvent(r, "admin_key_revoke", rec.UserID, rec.Owner, map[string]any{
			"owner": rec.Owner, "key_id": rec.ID, "key_name": rec.Name, "key_label": keyLabel(rec.Last4), "scope": rec.Scope,
		}))
	}
	if err == nil {
		err = audit.Commit(tx)
	}
	if err != nil {
		log.Printf("admin: revoke leaked key %s of %s: %v", rec.ID, rec.Owner, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out.Status = "revoked"
	out.RevokedAt = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, out)
}

// leakedKeyCardHTML is the /admin card for revoking a leaked key. The key is
// typed into a password field so it is not left on screen, and sent by
// adminLeakedKeyScript as JSON; the answer names whose key it was.
const leakedKeyCardHTML = `
<section class="overview" id="admin-leaked-key">
  <div class="overview-card"><h2 class="section-title">Revoke a leaked key</h2>
    <p class="login-copy">Found an API key in a paste, a log or a commit? Paste it here. That one key stops working at once; its owner keeps their other keys and their sign-in.</p>
    <form id="leaked-key-form" class="login-form" onsubmit="return false">
      <input type="password" id="leaked-key" placeholder="shk_…" autocomplete="off" spellcheck="false" aria-label="Leaked API key">
      <button type="button" id="leaked-key-button" class="btn-reject">Revoke</button>
    </form>
    <p id="leaked-key-result" class="login-copy" role="status" aria-live="polite"></p>
  </div>
</section>`

const adminLeakedKeyScript = `<script>
(function(){
  var button = document.getElementById('leaked-key-button');
  var input = document.getElementById('leaked-key');
  var out = document.getElementById('leaked-key-result');
  if (!button) return;
  button.addEventListener('click', function(){
    var key = input.value.trim();
    if (!key) { out.textContent = 'Paste the key first.'; return; }
    button.disabled = true;
    fetch('/api/admin/keys/revoke', {method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json', 'X-Simple-Host-Client': 'control-ui'},
      body: JSON.stringify({key: key})})
      .then(function(r){ return r.json().then(function(body){ return {ok: r.ok, body: body}; }); })
      .then(function(res){
        input.value = '';
        var b = res.body || {};
        if (!res.ok) { out.textContent = b.error || 'Could not revoke that key.'; return; }
        var which = b.owner + '’s key “' + b.name + '” (' + b.label + ')';
        out.textContent = b.status === 'revoked' ? 'Revoked ' + which + '.' : which + ' was already revoked.';
      })
      .catch(function(){ out.textContent = 'Could not reach the server.'; })
      .then(function(){ button.disabled = false; });
  });
})();
</script>`

// registerKeyRoutes wires the admin's leaked-key revoke: a browser session
// only, Origin-checked with a JSON refusal (it is called by fetch).
func (h *AdminHandler) registerKeyRoutes(mux *http.ServeMux, adminAPI func(http.Handler) http.Handler) {
	mux.Handle("POST /api/admin/keys/revoke", originCheckMiddleware(h.hosts, h.publicBaseURL)(adminAPI(http.HandlerFunc(h.revokeLeakedKey))))
}
