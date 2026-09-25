package handler

import (
	"context"

	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// auditActorKind reports the actor_kind for the request's authenticated
// principal ("person" for a session, "key" for an X-API-Key)
// and the key id when it is a key, mirroring how site_api.go's siteAPICall
// already derives ActorKind from auth.APIKeyID for the site-facing API.
func auditActorKind(ctx context.Context) (kind, keyID string) {
	if id := auth.APIKeyID(ctx); id != "" {
		return "key", id
	}
	return "person", ""
}

// auditRequestID reads the id reqlog.Middleware assigned this request, so an
// audit_events row can be tied back to the structured request log line.
// Returns "" outside the middleware — a handler unit test
// that builds its own *http.Request rather than going through the mux, for
// instance — rather than panicking.
func auditRequestID(ctx context.Context) string {
	if record := reqlog.FromContext(ctx); record != nil {
		return record.ID
	}
	return ""
}
