package audit

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/reqlog"
)

// Compile-time check that both Recorder implementations in this package
// (NoOp and DBRecorder) actually satisfy the widened interface — the point
// of widening it with RecordTx rather than adding a second interface.
var (
	_ Recorder = NoOp{}
	_ Recorder = (*DBRecorder)(nil)
)

func TestNoOpRecordTxNeverErrorsAndDoesNotRequireATx(t *testing.T) {
	if err := (NoOp{}).RecordTx(context.Background(), nil, Event{Action: "sign_in"}); err != nil {
		t.Fatalf("NoOp.RecordTx(nil tx) = %v, want nil", err)
	}
}

func TestEventDetailJSONPrecedence(t *testing.T) {
	tests := []struct {
		name string
		e    Event
		want map[string]any
	}{
		{name: "nothing set", e: Event{Action: "sign_in"}, want: nil},
		{
			name: "detail only",
			e:    Event{Action: "key_mint", Detail: "prefix abc123"},
			want: map[string]any{"note": "prefix abc123"},
		},
		{
			name: "subject only",
			e:    Event{Action: "admin_disable_user", SubjectID: "user-1"},
			want: map[string]any{"subject_id": "user-1"},
		},
		{
			name: "extra cannot shadow the fixed keys",
			e: Event{
				Action:    "admin_disable_user",
				SubjectID: "user-1",
				Detail:    "note",
				Extra:     map[string]any{"subject_id": "should not win", "note": "should not win either", "count": 3},
			},
			want: map[string]any{"subject_id": "user-1", "note": "note", "count": 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.e.detailJSON()
			if len(got) != len(test.want) {
				t.Fatalf("detailJSON() = %#v, want %#v", got, test.want)
			}
			for k, v := range test.want {
				if got[k] != v {
					t.Errorf("detailJSON()[%q] = %#v, want %#v", k, got[k], v)
				}
			}
		})
	}
}

func TestEventRowDefaultsActorKindToPerson(t *testing.T) {
	row := Event{Action: "sign_in"}.row(time.Unix(0, 0))
	if row.ActorKind != "person" {
		t.Fatalf("ActorKind = %q, want %q", row.ActorKind, "person")
	}
}

func TestEventRowKeepsExplicitActorKind(t *testing.T) {
	row := Event{Action: "state_write", ActorKind: "key"}.row(time.Unix(0, 0))
	if row.ActorKind != "key" {
		t.Fatalf("ActorKind = %q, want %q", row.ActorKind, "key")
	}
}

func TestCoalesceWindowTruncatesToAnAbsoluteBoundary(t *testing.T) {
	// Two calls straddling the same five-minute boundary, from two
	// independent goroutines with no shared "first call" reference, must
	// truncate to the identical instant for audit_bump_state_write's
	// upsert to coalesce them into one row (design 8.1).
	a := time.Date(2026, 9, 17, 14, 3, 10, 0, time.UTC)
	b := time.Date(2026, 9, 17, 14, 4, 59, 999000000, time.UTC)
	wantWindow := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	if got := a.UTC().Truncate(coalesceWindow); !got.Equal(wantWindow) {
		t.Fatalf("window(a) = %v, want %v", got, wantWindow)
	}
	if got := b.UTC().Truncate(coalesceWindow); !got.Equal(wantWindow) {
		t.Fatalf("window(b) = %v, want %v", got, wantWindow)
	}
	c := time.Date(2026, 9, 17, 14, 5, 0, 0, time.UTC)
	if got := c.UTC().Truncate(coalesceWindow); got.Equal(wantWindow) {
		t.Fatalf("window(c) = %v, want it to fall in the next window, not %v", got, wantWindow)
	}
}

func TestNewDBRecorderPanicsOnNilDatabase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewDBRecorder(nil) did not panic")
		}
	}()
	NewDBRecorder((*sql.DB)(nil))
}

func TestNewReaderPanicsOnNilDatabase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewReader(nil) did not panic")
		}
	}()
	NewReader((*sql.DB)(nil))
}

func TestWithRequestInfoFillsFromTheRequestLog(t *testing.T) {
	var got Event
	handler := reqlog.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = withRequestInfo(r.Context(), Event{Action: "key_mint"})
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/keys", nil)
	request.RemoteAddr = "192.0.2.7:1234"
	request.Header.Set("User-Agent", "ci-runner/1")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if got.IP != "192.0.2.7" || got.UserAgent != "ci-runner/1" || got.RequestID == "" {
		t.Fatalf("event = %+v", got)
	}
	if kept := withRequestInfo(context.Background(), Event{IP: "x"}); kept.IP != "x" {
		t.Fatalf("outside a request: %+v", kept)
	}
}
