package mcp

import "github.com/vsriram/simple-host/internal/notifyevents"

type EventsBackend = notifyevents.EventsBackend
type EventSubscribeRequest = notifyevents.EventSubscribeRequest
type EventSubscribeResult = notifyevents.EventSubscribeResult
type EventUnsubscribeRequest = notifyevents.EventUnsubscribeRequest
type EventError = notifyevents.EventError
type Caller = notifyevents.Caller

const (
	EventCodeNotFound          = notifyevents.EventCodeNotFound
	EventCodeForbidden         = notifyevents.EventCodeForbidden
	EventCodeResourceExhausted = notifyevents.EventCodeResourceExhausted
	EventCodeUnsupported       = notifyevents.EventCodeUnsupported
	EventCodeCallbackEndpoint  = notifyevents.EventCodeCallbackEndpoint
)
