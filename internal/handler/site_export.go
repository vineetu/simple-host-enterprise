package handler

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// An owner or team member downloads a whole site (live files, current saved
// data and its history, versions and assets) in two steps: POST export-link
// returns a short-lived signed address, and GET on that address streams the
// zip. The browser and an agent (MCP export_site) use the same two steps,
// so a download link can be handed to someone's browser without it carrying
// the caller's key or cookie. The zip is written by writeSiteExport, the
// code the admin's person export uses.

// siteExportLinkTTL is how long a download address works.
const siteExportLinkTTL = 10 * time.Minute

// siteExportTimeout bounds one download stream.
const siteExportTimeout = 10 * time.Minute

// siteExportDomain separates export-link signatures from session cookies,
// which are signed with the same keys: a token of one kind never verifies
// as the other.
const siteExportDomain = "simple-host site export v1\n"

type siteExportClaims struct {
	SiteID  string `json:"sid"`
	ActorID string `json:"uid"`
	Owner   string `json:"own"`
	Site    string `json:"site"`
	Exp     int64  `json:"exp"`
}

type siteExportLinkResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WithSigningKeys sets the keys preview and export links are signed with (the session
// signing keys, rotated the same way).
func (h *SiteHandler) WithSigningKeys(keys []auth.SigningKey) *SiteHandler {
	h.signingKeys = keys
	return h
}

func (h *SiteHandler) registerExportRoutes(mux *http.ServeMux, ownerMutation, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/export-link", browserWrite(ownerMutation(http.HandlerFunc(h.siteExportLink))))
	// The token is the credential: no key, no cookie.
	mux.HandleFunc("GET /api/site-export/{token}", h.downloadSiteExport)
}

func signSiteExport(keys []auth.SigningKey, claims siteExportClaims) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("no signing key configured")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, keys[0].Key)
	mac.Write([]byte(siteExportDomain + encoded))
	return keys[0].ID + "." + encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

var errSiteExportToken = errors.New("invalid or expired download link")

func verifySiteExport(keys []auth.SigningKey, token string, now time.Time) (siteExportClaims, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return siteExportClaims{}, errSiteExportToken
	}
	var key []byte
	for _, k := range keys {
		if k.ID == parts[0] {
			key = k.Key
			break
		}
	}
	if key == nil {
		return siteExportClaims{}, errSiteExportToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return siteExportClaims{}, errSiteExportToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(siteExportDomain + parts[1]))
	if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return siteExportClaims{}, errSiteExportToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return siteExportClaims{}, errSiteExportToken
	}
	var claims siteExportClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.SiteID == "" || claims.ActorID == "" {
		return siteExportClaims{}, errSiteExportToken
	}
	if !now.Before(time.Unix(claims.Exp, 0)) {
		return siteExportClaims{}, errSiteExportToken
	}
	return claims, nil
}

// siteExportLink mints a download address for the site, for its owner or a
// member of the owning team.
func (h *SiteHandler) siteExportLink(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	expires := time.Now().Add(siteExportLinkTTL).UTC().Truncate(time.Second)
	token, err := signSiteExport(h.signingKeys, siteExportClaims{
		SiteID: access.Site.ID, ActorID: access.ActorID, Owner: access.OwnerUsername, Site: access.Site.Name, Exp: expires.Unix(),
	})
	if err != nil {
		log.Printf("sign export link %s/%s: %v", ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, siteExportLinkResponse{
		URL:       strings.TrimRight(h.publicBaseURL, "/") + "/api/site-export/" + token,
		ExpiresAt: expires,
	})
}

// downloadSiteExport streams the zip. The caller named in the link must
// still be able to manage the same site (same id, so a site deleted and
// re-created under the name is refused) and still be allowed to sign in.
func (h *SiteHandler) downloadSiteExport(w http.ResponseWriter, r *http.Request) {
	claims, err := verifySiteExport(h.signingKeys, r.PathValue("token"), time.Now())
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	disabled, err := db.IsUserDisabled(r.Context(), h.database, claims.ActorID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("export: check actor %s: %v", claims.ActorID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if disabled || errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: errSiteExportToken.Error()})
		return
	}
	access, err := db.ResolveSiteAccess(r.Context(), h.database, claims.ActorID, claims.Owner, claims.Site)
	if err != nil || access.Site.ID != claims.SiteID || !grantsOwnerRole(access.Role) {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("export: resolve %s/%s: %v", claims.Owner, claims.Site, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: errSiteExportToken.Error()})
		return
	}
	release, acquired := h.limits.acquireArchiveDownload()
	if !acquired {
		writeConcurrencyRateLimit(w)
		return
	}
	defer release()
	h.audit.Record(r.Context(), audit.Event{
		ActorID: claims.ActorID, ActorKind: "person",
		Action: "site_export", OwnerID: access.OwnerID, SiteID: access.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"active_version": access.Site.ActiveVersion, "via": "download_link"},
	})

	deadline := time.Now().Add(siteExportTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": access.OwnerUsername + "-" + access.Site.Name + ".zip",
	}))
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	if err := writeSiteExport(ctx, zw, h.database, h.store, access.Site.ID, access.Site.ActiveVersion, ""); err != nil {
		// The status is already sent: a truncated zip fails to open.
		log.Printf("export %s/%s: %v", claims.Owner, claims.Site, err)
		return
	}
	if err := zw.Close(); err != nil {
		log.Printf("finish export %s/%s: %v", claims.Owner, claims.Site, err)
	}
}

func grantsOwnerRole(role db.CollaborationRole) bool {
	return role == db.CollaborationRoleOwner || role == db.CollaborationRoleMember
}
