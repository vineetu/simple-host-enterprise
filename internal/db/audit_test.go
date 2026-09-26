package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestListAuditEventsQueryShape(t *testing.T) {
	for _, required := range []string{
		"owner_id = ANY($1::uuid[]) OR team_id = ANY($1::uuid[])",
		"($2 = '' OR owner_id = $2::uuid)",
		"($3 = '' OR site_id = $3::uuid)",
		"($4 = '' OR actor_id = $4::uuid)",
		"($5 = '' OR action = $5)",
		"(at, id) < ($8, $9)",
		"ORDER BY at DESC, id DESC",
	} {
		if !strings.Contains(listAuditEventsQuery, required) {
			t.Errorf("listAuditEventsQuery does not contain %q: %s", required, listAuditEventsQuery)
		}
	}
}

func TestListAccessLogQueryShape(t *testing.T) {
	for _, required := range []string{
		"($1 = '' OR owner_label = $1)",
		"($2 = '' OR site_name = $2)",
		"(at, id) < ($5, $6)",
		"ORDER BY at DESC, id DESC",
	} {
		if !strings.Contains(listAccessLogQuery, required) {
			t.Errorf("listAccessLogQuery does not contain %q: %s", required, listAccessLogQuery)
		}
	}
}

func TestListAuditEventsNilDatabaseReturnsEmpty(t *testing.T) {
	page, err := ListAuditEvents(context.Background(), nil, AuditEventFilter{Admin: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.NextCursor != "" {
		t.Fatalf("nil database returned %#v, want an empty page", page)
	}
}

func TestListAuditEventsRequiresOwnerScopeUnlessAdmin(t *testing.T) {
	if _, err := ListAuditEvents(context.Background(), nil, AuditEventFilter{}, ""); err == nil {
		t.Fatal("ListAuditEvents with no OwnerScope and Admin=false should be refused before it ever reaches nil-db's early return")
	}
	if _, err := ListAuditEvents(context.Background(), nil, AuditEventFilter{OwnerScope: []string{"owner-1"}}, ""); err != nil {
		t.Fatalf("ListAuditEvents with a non-empty OwnerScope should be accepted: %v", err)
	}
	if _, err := ListAuditEvents(context.Background(), nil, AuditEventFilter{Admin: true}, ""); err != nil {
		t.Fatalf("ListAuditEvents with Admin=true and no OwnerScope should be accepted: %v", err)
	}
}

func TestListAccessLogNilDatabaseReturnsEmpty(t *testing.T) {
	page, err := ListAccessLog(context.Background(), nil, AccessLogFilter{Admin: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 0 || page.NextCursor != "" {
		t.Fatalf("nil database returned %#v, want an empty page", page)
	}
}

func TestListAccessLogRequiresOwnerUnlessAdmin(t *testing.T) {
	if _, err := ListAccessLog(context.Background(), nil, AccessLogFilter{}, ""); err == nil {
		t.Fatal("ListAccessLog with no Owner and Admin=false should be refused before it ever reaches nil-db's early return")
	}
}

func TestAuditCursorRoundTrips(t *testing.T) {
	want := auditCursor{At: time.Date(2026, 9, 17, 12, 30, 0, 123456000, time.UTC), ID: 42}
	got, err := decodeAuditCursor(encodeAuditCursor(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.Equal(want.At) || got.ID != want.ID {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestDecodeAuditCursorEmptyIsZero(t *testing.T) {
	got, err := decodeAuditCursor("")
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.IsZero() || got.ID != 0 {
		t.Fatalf("decodeAuditCursor(\"\") = %+v, want the zero value", got)
	}
}

func TestDecodeAuditCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-base64!!", "aGVsbG8", "aGVsbG98d29ybGQ"} {
		if _, err := decodeAuditCursor(bad); err == nil {
			t.Errorf("decodeAuditCursor(%q) accepted, want an error", bad)
		}
	}
}

func TestInsertAuditEventRequiresAction(t *testing.T) {
	if _, _, err := InsertAuditEvent(context.Background(), nil, AuditEvent{}); err == nil {
		t.Fatal("InsertAuditEvent with no Action should be refused before it ever touches q")
	}
}

func TestBumpStateWriteRequiresWindowStart(t *testing.T) {
	if err := BumpStateWrite(context.Background(), nil, BumpStateWriteParams{}); err == nil {
		t.Fatal("BumpStateWrite with a zero WindowStart should be refused before it ever touches q")
	}
}

func TestInsertAccessLogBatchEmptyIsNoop(t *testing.T) {
	if err := InsertAccessLogBatch(context.Background(), nil, nil); err != nil {
		t.Fatalf("InsertAccessLogBatch(nil) = %v, want nil (a no-op, never touching q)", err)
	}
}
