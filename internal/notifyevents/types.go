package notifyevents

import (
	"context"
	"time"
)

// EventsBackend stores and removes MCP Events webhook subscriptions (the
// io.modelcontextprotocol/events extension, webhook mode). The handler
// package implements it; this package validates the request shape, the event
// name and the whsec_ secret format before calling it. A nil backend
// means the server does not offer events.
type EventsBackend interface {
	// Subscribe creates or refreshes the subscription keyed by
	// (c.Owner, c.OAuthGrantID, req.URL, req.Name, req.CanonicalArgs). It verifies the
	// callback endpoint (signed verification challenge) before a new key
	// becomes active. Errors that are not *EventError are internal errors.
	Subscribe(ctx context.Context, c Caller, req EventSubscribeRequest) (EventSubscribeResult, error)
	// Unsubscribe removes the subscription with that key. Removing one that
	// does not exist succeeds.
	Unsubscribe(ctx context.Context, c Caller, req EventUnsubscribeRequest) error
}

// EventSubscribeRequest is a validated events/subscribe call.
type EventSubscribeRequest struct {
	// Name is one of the advertised event types (record.added,
	// record.changed, file.uploaded, file.deleted).
	Name string
	// Site is arguments.site, the site's name as the site tools use it.
	Site  string
	Owner string
	// Storage is arguments.storage, "" for every storage resource on the site.
	Storage string
	// CanonicalArgs is the arguments object as canonical JSON (sorted keys,
	// no insignificant whitespace), part of the subscription key.
	CanonicalArgs string
	// URL is delivery.url, not yet checked for HTTPS or a public address.
	URL string
	// Secret is delivery.secret: "whsec_" + base64 of 24 to 64 bytes.
	Secret string
	// Cursor is the client's last persisted cursor, nil for "from now".
	Cursor *string
	// TTLSet is true when the request carried ttlMs. TTLSet with a nil TTL
	// asks for no expiry; !TTLSet asks for the server default.
	TTLSet bool
	TTL    *time.Duration
}

// EventSubscribeResult is the events/subscribe answer.
type EventSubscribeResult struct {
	ID            string
	RefreshBefore *time.Time // nil = no expiry
	Cursor        *string
	Truncated     bool
	// Refreshed is true when the key already existed; the response then
	// carries deliveryStatus {active, lastError}.
	Refreshed bool
	Active    bool
	LastError string // a category, never remote text
}

// EventUnsubscribeRequest is a validated events/unsubscribe call.
type EventUnsubscribeRequest struct {
	Name          string
	Site          string
	Owner         string
	Storage       string
	CanonicalArgs string
	URL           string
}

// The client uses the earlier draft error numbers. A later MCP Events draft
// renumbered these to -32023 through -32027; keep the real client contract.
const (
	EventCodeNotFound          = -32011
	EventCodeForbidden         = -32012
	EventCodeResourceExhausted = -32013
	EventCodeUnsupported       = -32014
	EventCodeCallbackEndpoint  = -32015
)

// EventError is a protocol error from the backend, returned to the client as
// a JSON-RPC error with this code, message and data.
type EventError struct {
	Code    int
	Message string
	Data    map[string]any
}

func (e *EventError) Error() string { return e.Message }

// Caller is the authenticated connector grant, never client-supplied metadata.
type Caller struct {
	Owner        string
	Username     string
	OAuthGrantID string
	AppName      string
	DeployOnly   bool
}
