package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// Preview before live: any kept version of a site, opened in a browser by its
// owner or a member of the owning team before (or instead of) making it live.
//
// The address is the site's own address plus "_preview/<token>/", where the
// token is "<version>-<expiry>-<signature>": an HMAC, under the session
// signing key, of the site id, the version and the expiry. It is minted by
// GET .../versions/{version}/preview for the owner or a team member and lasts
// previewTTL. The link alone opens nothing: the host gate still requires a
// host session, and that person must be the site's owner or in its team
// (previewAllowed), whatever the site's access level. A preview is never
// indexed or cached, and the site API refuses a save from a preview page.
// Making the version live is the ordinary rollback to it.

const previewTTL = time.Hour

// previewSegment is the path segment a preview lives under. A signature that
// does not verify is not a preview, so a site's own "_preview" folder is
// still served as usual.
const previewSegment = "_preview"

func previewMAC(key []byte, siteID string, version int, expires int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("simple-host preview v1\x00" + siteID + "\x00" + strconv.Itoa(version) + "\x00" + strconv.FormatInt(expires, 10)))
	return mac.Sum(nil)
}

// previewToken signs version of siteID until expires with the current
// signing key (the first).
func previewToken(keys []auth.SigningKey, siteID string, version int, expires time.Time) string {
	sig := previewMAC(keys[0].Key, siteID, version, expires.Unix())
	return strconv.Itoa(version) + "-" + strconv.FormatInt(expires.Unix(), 10) + "-" + base64.RawURLEncoding.EncodeToString(sig)
}

// parsePreviewToken reports the version and expiry a token was signed for,
// when its signature verifies for siteID under any configured key (so a key
// rotation does not end live links early). Expiry is the caller's to check.
func parsePreviewToken(keys []auth.SigningKey, siteID, token string) (version int, expires time.Time, ok bool) {
	parts := strings.SplitN(token, "-", 3)
	if len(parts) != 3 || len(keys) == 0 {
		return 0, time.Time{}, false
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil || version < 1 {
		return 0, time.Time{}, false
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, time.Time{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, time.Time{}, false
	}
	for _, key := range keys {
		if hmac.Equal(sig, previewMAC(key.Key, siteID, version, unix)) {
			return version, time.Unix(unix, 0), true
		}
	}
	return 0, time.Time{}, false
}

// WithPreviewKeys sets the keys preview links are signed with (the session
// signing keys) and returns h for chaining. Without them the preview route
// answers 503.
func (h *SiteHandler) WithPreviewKeys(keys []auth.SigningKey) *SiteHandler {
	h.previewKeys = keys
	return h
}

func (h *SiteHandler) registerPreviewRoutes(mux *http.ServeMux, ownerMutation func(http.Handler) http.Handler) {
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/preview", ownerMutation(http.HandlerFunc(h.previewVersion)))
}

type previewResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Version   int       `json:"version"`
	Live      bool      `json:"live"`
}

// previewVersion mints a preview link for one kept version.
func (h *SiteHandler) previewVersion(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || number < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid version"})
		return
	}
	if len(h.previewKeys) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "previews are not available on this server"})
		return
	}
	versions, err := db.ListVersions(r.Context(), h.database, access.Site.ID)
	if err != nil {
		log.Printf("preview: list versions for %s/%s: %v", ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !versionExists(versions, number) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "version not found"})
		return
	}
	base := h.siteURL(r.Context(), access.OwnerUsername, access.Site.Name, access.Site.ID)
	if base == "" {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site has no address"})
		return
	}
	expires := time.Now().Add(previewTTL).Truncate(time.Second).UTC()
	writeJSON(w, http.StatusOK, previewResponse{
		URL:       base + previewSegment + "/" + previewToken(h.previewKeys, access.Site.ID, number, expires) + "/",
		ExpiresAt: expires,
		Version:   number,
		Live:      number == access.Site.ActiveVersion,
	})
}

// servePreview answers a request whose path, below the site's own mount
// (prefix: "" on the site's host, "/<site>" on the fallback owner path), is
// rest = "_preview/<token>/...". false means it is not a preview (the
// signature does not verify) and the request is served as ordinary content.
func (g *hostGate) servePreview(w http.ResponseWriter, r *http.Request, owner, siteName, siteID, requestHost, prefix, rest string) bool {
	token, _, hasSlash := strings.Cut(rest, "/")
	version, expires, ok := parsePreviewToken(g.signingKeys, siteID, token)
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "This preview link has expired. Open a new one from the dashboard, or ask your agent for a fresh one.", http.StatusGone)
		return true
	}
	if !hasSlash {
		redirectWithTrailingSlash(w, r)
		return true
	}
	// A preview is for the people who can publish the site, whoever else can
	// open it: a real host session, never a network visitor.
	userID, sessionID, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return true
	}
	allowed := false
	if g.previewAllowed != nil {
		var err error
		allowed, err = g.previewAllowed(r, siteID, userID, version)
		if err != nil {
			log.Printf("host gate: preview check for site %s v%d: %v", siteID, version, err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return true
		}
	}
	if !allowed {
		g.recordDenied(r, siteID, userID, "not_owner_preview")
		http.NotFound(w, r)
		return true
	}
	g.files.serveVersion(w, r, owner, siteName, siteID, version, prefix+"/"+previewSegment+"/"+token, userID, sessionID)
	return true
}

// fromPreview reports whether a request was made by a page opened as a
// preview of siteID: its Referer is on this host and under a preview path
// whose signature verifies (expired or not). Used to refuse saves, so a
// version that is not live cannot change the live site's data.
func (g *hostGate) fromPreview(r *http.Request, siteID string) bool {
	ref, err := neturl.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host == "" || !strings.EqualFold(ref.Host, r.Host) {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(ref.Path, "/"), "/")
	for i := 0; i < len(segments)-1 && i < 2; i++ {
		if segments[i] == previewSegment {
			if _, _, ok := parsePreviewToken(g.signingKeys, siteID, segments[i+1]); ok {
				return true
			}
		}
	}
	return false
}

// serveVersion serves one kept version of a site beneath prefix, for a
// preview: never cached, never indexed, and not counted as a visit (the
// access log still records it, as it does every hosted-content response).
func (s *SiteFiles) serveVersion(w http.ResponseWriter, r *http.Request, owner, siteName, siteID string, version int, prefix, userID, sessionID string) {
	openCtx, cancel := context.WithTimeout(r.Context(), siteOpenTimeout)
	root, err := s.store.OpenVersion(openCtx, siteID, version)
	cancel()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		log.Printf("open version %s v%d for preview: %v", siteID, version, err)
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "private, no-store")
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	http.StripPrefix(prefix, http.FileServer(noDirListingFS{fs: http.FS(root.FS())})).ServeHTTP(recorder, r)
	s.access.Enqueue(audit.AccessEvent{
		At:         time.Now().UTC(),
		UserID:     userID,
		SessionID:  sessionID,
		OwnerLabel: ownerLabel(owner),
		SiteName:   siteName,
		Path:       r.URL.Path,
		Method:     r.Method,
		Status:     recorder.status,
		Bytes:      recorder.bytes,
		IP:         reqlog.ClientIP(r),
		UserAgent:  r.UserAgent(),
		ClientKind: classifyClient(r).String(),
	})
}
