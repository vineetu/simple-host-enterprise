package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type testEvents struct {
	caller      Caller
	subscribe   EventSubscribeRequest
	unavailable bool
}

func (b *testEvents) EventsAvailability() error {
	if b.unavailable {
		return &EventError{Code: EventCodeForbidden, Message: "disabled"}
	}
	return nil
}
func (b *testEvents) Subscribe(ctx context.Context, c Caller, r EventSubscribeRequest) (EventSubscribeResult, error) {
	b.caller = c
	b.subscribe = r
	cursor := "c_1"
	return EventSubscribeResult{ID: "sub_1", Cursor: &cursor, Refreshed: true, Active: true}, nil
}
func (b *testEvents) Unsubscribe(context.Context, Caller, EventUnsubscribeRequest) error { return nil }
func TestEventsProtocol(t *testing.T) {
	backend := &testEvents{}
	s := NewServer(nil, "test", "test").WithEvents(backend)
	c := Caller{Owner: "person", OAuthGrantID: "grant", Username: "alice"}
	if !s.offersEvents(c) {
		t.Fatal("enabled events hidden")
	}
	for _, caller := range []Caller{{Owner: "person"}, {Owner: "person", OAuthGrantID: "grant", DeployOnly: true}} {
		if s.offersEvents(caller) {
			t.Fatal("unbound events offered")
		}
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	req := request{ID: json.RawMessage("1"), Method: "events/list"}
	out := s.dispatchEvents(r, req, c, true)
	if out.Error != nil {
		t.Fatal(out.Error)
	}
	definitions := out.Result.(map[string]any)["events"].([]map[string]any)
	if len(definitions) != 4 {
		t.Fatal("event definitions missing")
	}
	req.Method = "events/subscribe"
	req.Params = json.RawMessage(`{"name":"file.deleted","arguments":{"site":"demo","owner":"team-work","storage":"uploads"},"delivery":{"mode":"webhook","url":"https://receiver.example/events","secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}}`)
	out = s.dispatchEvents(r, req, c, true)
	if out.Error != nil {
		t.Fatal(out.Error)
	}
	if backend.caller.OAuthGrantID != "grant" || backend.subscribe.Owner != "team-work" {
		t.Fatal("identity or namespace lost")
	}
	if _, ok := out.Result.(map[string]any)["deliveryStatus"]; !ok {
		t.Fatal("refresh status absent")
	}
	backend.unavailable = true
	if s.offersEvents(c) {
		t.Fatal("keyless events offered")
	}
	if out = s.dispatchEvents(r, req, c, true); out.Error == nil || out.Error.Code != EventCodeForbidden {
		t.Fatal("keyless subscribe accepted")
	}
}
func TestEventsValidationAndRedaction(t *testing.T) {
	for _, raw := range []string{`{}`, `{"name":"record.added","arguments":{"site":"demo","unknown":"x"}}`, `{"name":"file.deleted","arguments":{"site":"demo"},"delivery":{"mode":"stream","url":"https://receiver.example"}}`} {
		if _, err := parseEventParams(json.RawMessage(raw), true); err == nil {
			t.Fatal("invalid subscription accepted")
		}
	}
	b, err := redactNotificationResult([]byte(`{"notifications":[{"url":"https://receiver.example/private?token=sensitive"}],"limits":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	if out["notifications"].([]any)[0].(map[string]any)["url"] != "https://receiver.example/..." {
		t.Fatal("callback path exposed")
	}
	s := NewServer(nil, "test", "test")
	for _, tool := range s.appTools(httptest.NewRequest("POST", "/mcp", nil)) {
		if isNotificationTool(tool.Name) {
			t.Fatal("notification tools advertised without backend")
		}
	}
}
