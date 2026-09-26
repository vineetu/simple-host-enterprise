package auth

import (
	"context"
	"net/http"

	"github.com/vsriram/simple-host/internal/db"
)

// Which API keys may call a route, by the pattern the mux matched
// (r.Pattern, set by http.ServeMux before the per-route middleware runs) or,
// for the host gate's site-facing API, SiteAPIPattern.
type keyAccess int

const (
	// keyNever: no API key of any scope. The admin routes, and every route
	// that never authenticates at all.
	keyNever keyAccess = iota
	// keyFull: a full key. Everything the person can do through the REST API.
	keyFull
	// keyPublish: a publish or a full key. What a CI job that publishes a
	// site needs, on sites the person can already write.
	keyPublish
	// keyOffboard: an offboard key only, and nothing else takes it.
	keyOffboard
)

// SiteAPIPattern stands in for a mux pattern when the host gate
// authenticates its site-facing API (a site's saved data and assets on an
// owner or restricted-site host, which is not served by the mux).
const SiteAPIPattern = "HOSTGATE /api/sites/{site}/{state|assets}"

// routeKeyAccess classifies every registered route. It is deny-by-default:
// a pattern missing from it is refused to every key, and
// TestEveryRouteIsClassified fails until a new route is added here, so a
// route can never become reachable by a publish key without someone
// deciding it should be.
var routeKeyAccess = map[string]keyAccess{
	// Publish: deploy, update, roll back, list; versions and archives;
	// saved data and assets; who am I; MCP.
	"GET /api/me":                                                                  keyPublish,
	"GET /api/sites":                                                               keyPublish,
	"POST /api/sites/{sitename}":                                                   keyPublish,
	"PUT /api/sites/{sitename}":                                                    keyPublish,
	"POST /api/sites/{sitename}/rollback":                                          keyPublish,
	"GET /api/sites/{sitename}/versions":                                           keyPublish,
	"GET /api/collaboration/sites":                                                 keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}":                              keyPublish,
	"POST /api/collaboration/sites/{owner}/{sitename}":                             keyPublish,
	"PUT /api/collaboration/sites/{owner}/{sitename}":                              keyPublish,
	"POST /api/collaboration/sites/{owner}/{sitename}/rollback":                    keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}/versions":                     keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/archive":   keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}/state-versions":               keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}":          keyPublish,
	"POST /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}/restore": keyPublish,
	"GET /api/collaboration/sites/{owner}/{sitename}/assets":                       keyPublish,
	"DELETE /api/collaboration/sites/{owner}/{sitename}/assets/{id}":               keyPublish,
	SiteAPIPattern: keyPublish,
	"POST /mcp":    keyPublish,
	"GET /mcp":     keyPublish,
	"DELETE /mcp":  keyPublish,

	// Full only: deleting a site, who can open it, viewers, teams, the
	// audit and access logs, search. A key on the audit and access logs
	// sees only its owner's own namespace and teams, even an admin's: the
	// company-wide view needs a browser session (handler/audit_access.go).
	"DELETE /api/sites/{sitename}":                                          keyFull,
	"DELETE /api/collaboration/sites/{owner}/{sitename}":                    keyFull,
	"POST /api/sites/{sitename}/access":                                     keyFull,
	"POST /api/collaboration/sites/{owner}/{sitename}/access":               keyFull,
	"GET /api/collaboration/sites/{owner}/{sitename}/viewers":               keyFull,
	"POST /api/collaboration/sites/{owner}/{sitename}/viewers":              keyFull,
	"DELETE /api/collaboration/sites/{owner}/{sitename}/viewers/{username}": keyFull,
	"GET /api/collaboration/sites/{owner}/{sitename}/viewer-candidates":     keyFull,
	"POST /api/teams":                             keyFull,
	"GET /api/teams":                              keyFull,
	"GET /api/teams/{team}/members":               keyFull,
	"GET /api/teams/{team}/member-candidates":     keyFull,
	"POST /api/teams/{team}/members":              keyFull,
	"DELETE /api/teams/{team}/members/{username}": keyFull,
	"POST /api/teams/{team}/leave":                keyFull,
	"DELETE /api/teams/{team}":                    keyFull,
	"GET /api/audit":                              keyFull,
	"GET /api/access":                             keyFull,
	"GET /api/search":                             keyFull,
	"POST /api/search/click":                      keyFull,
	// Session-only routes: the handler refuses every key itself, with a
	// message that says a browser session is needed; full lets that
	// message through rather than a scope refusal.
	"GET /api/keys":                   keyFull,
	"POST /api/keys":                  keyFull,
	"DELETE /api/keys/{id}":           keyFull,
	"GET /auth/handoff":               keyFull,
	"GET /auth/sessions":              keyFull,
	"POST /auth/sessions/{id}/revoke": keyFull,
	"POST /auth/logout":               keyFull,
	"POST /oauth/authorize":           keyFull,

	// Offboarding: the one thing an offboard key does.
	"POST /api/admin/users/disable": keyOffboard,

	// Never a key: administration, and routes that authenticate nobody.
	"GET /admin": keyNever,
	"POST /api/admin/users/{username}/disable":                   keyNever,
	"POST /api/admin/users/{username}/enable":                    keyNever,
	"POST /api/admin/teams/{team}/delete":                        keyNever,
	"GET /api/admin/export":                                      keyNever,
	"POST /api/admin/access-requests/{owner}/{sitename}/approve": keyNever,
	"POST /api/admin/access-requests/{owner}/{sitename}/decline": keyNever,
	"POST /api/admin/access-requests/{owner}/{sitename}/revoke":  keyNever,
	"GET /":                                  keyNever,
	"GET /healthz":                           keyNever,
	"GET /readyz":                            keyNever,
	"GET /metrics":                           keyNever,
	"GET /dashboard":                         keyNever,
	"GET /showcase":                          keyNever,
	"GET /plugin.zip":                        keyNever,
	"GET /skills.zip":                        keyNever,
	"GET /skills/version":                    keyNever,
	"GET /skills/sha256/{digest}/skills.zip": keyNever,
	"GET /auth/login":                        keyNever,
	"GET /auth/callback":                     keyNever,
	"GET /.well-known/oauth-protected-resource":     keyNever,
	"GET /.well-known/oauth-protected-resource/mcp": keyNever,
	"GET /.well-known/oauth-authorization-server":   keyNever,
	"POST /oauth/register":                          keyNever,
	"GET /oauth/authorize":                          keyNever,
	"POST /oauth/token":                             keyNever,
	"POST /oauth/revoke":                            keyNever,
}

// KeyScopeAllows reports whether an API key of scope may call the route
// registered as pattern.
func KeyScopeAllows(scope, pattern string) bool {
	access, ok := routeKeyAccess[pattern]
	if !ok {
		return false
	}
	switch scope {
	case db.APIKeyScopePublish:
		return access == keyPublish
	case db.APIKeyScopeFull:
		return access == keyPublish || access == keyFull
	case db.APIKeyScopeOffboard:
		return access == keyOffboard
	}
	return false
}

// requestPattern is what the scope table is keyed on for r.
func requestPattern(r *http.Request) string {
	if isSiteAPI(r.Context()) {
		return SiteAPIPattern
	}
	return r.Pattern
}

type markKey int

const (
	viaMCPKey markKey = iota
	siteAPIKey
)

// WithMCPCaller marks ctx as a request that arrived on /mcp. Only
// connector.ProtectMCP sets it; the MCP server's in-process calls to the
// REST routes and the site API inherit it. A context value cannot be sent by
// a client, so an OAuth access token presented anywhere else never carries
// it and is refused (see Middleware).
func WithMCPCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, viaMCPKey, true)
}

func viaMCP(ctx context.Context) bool {
	v, _ := ctx.Value(viaMCPKey).(bool)
	return v
}

// WithSiteAPI marks ctx as the host gate authenticating its site-facing API,
// so the scope table classifies it as SiteAPIPattern.
func WithSiteAPI(ctx context.Context) context.Context {
	return context.WithValue(ctx, siteAPIKey, true)
}

func isSiteAPI(ctx context.Context) bool {
	v, _ := ctx.Value(siteAPIKey).(bool)
	return v
}
