package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessHandlerRequiresDatabaseAndSchema(t *testing.T) {
	tests := []struct {
		name             string
		pingErr          error
		schemaErr        error
		wantStatus       int
		wantSchemaChecks int
	}{
		{name: "ready", wantStatus: http.StatusOK, wantSchemaChecks: 1},
		{name: "database unavailable", pingErr: errors.New("down"), wantStatus: http.StatusServiceUnavailable},
		{name: "schema unavailable", schemaErr: errors.New("missing migration"), wantStatus: http.StatusServiceUnavailable, wantSchemaChecks: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaChecks := 0
			handler := readinessHandler(
				func(context.Context) error { return test.pingErr },
				func(context.Context) error {
					schemaChecks++
					return test.schemaErr
				},
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if schemaChecks != test.wantSchemaChecks {
				t.Fatalf("schema checks = %d, want %d", schemaChecks, test.wantSchemaChecks)
			}
			wantBody := `"status":"ok"`
			if test.wantStatus != http.StatusOK {
				wantBody = `"status":"unready"`
			}
			if !strings.Contains(response.Body.String(), wantBody) {
				t.Fatalf("body = %q, want %s", response.Body.String(), wantBody)
			}
		})
	}
}

func TestReadinessHandlerCachesTheResult(t *testing.T) {
	checks := 0
	handler := readinessHandler(
		func(context.Context) error { return nil },
		func(context.Context) error { checks++; return nil },
	)
	for i := 0; i < 5; i++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.Code)
		}
	}
	if checks != 1 {
		t.Fatalf("schema probe ran %d times within the cache window, want 1", checks)
	}
}

func TestReadinessHandlerRejectsFalseSchemaProbe(t *testing.T) {
	handler := readinessHandler(
		func(context.Context) error { return nil },
		func(context.Context) error { return requireSchemaReady(false) },
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(response.Body.String(), `"status":"unready"`) {
		t.Fatalf("body = %q, want unready status", response.Body.String())
	}
}

func TestRequiredSchemaProbeValidatesMigrations0009And0010Shape(t *testing.T) {
	for _, required := range []string{
		"information_schema.columns",
		"table_schema = 'public'",
		"table_name = 'sites'",
		"('uses_state', 'bool', 'false')",
		"('uses_versioned_state', 'bool', 'false')",
		"table_name = 'site_file_downloads'",
		"('site_id', 'uuid')",
		"('path', 'text')",
		"('day', 'date')",
		"('downloads', 'int8')",
		"('first_seen_at', 'timestamptz')",
		"('last_seen_at', 'timestamptz')",
		"is_nullable = 'NO'",
		"column_default = expected.column_default",
		"pg_catalog.pg_index",
		"index_catalog.indisvalid",
		"index_catalog.indisready",
		"index_catalog.indisunique OR index_catalog.indisprimary",
		"index_catalog.indpred IS NULL",
		"index_catalog.indnkeyatts = 3",
		"WITH ORDINALITY",
		"ARRAY['site_id', 'path', 'day']",
	} {
		if !strings.Contains(requiredSchemaProbe, required) {
			t.Errorf("schema probe does not reference %q", required)
		}
	}
	if strings.Contains(requiredSchemaProbe, "LIMIT 0") {
		t.Error("schema probe still uses a name-only LIMIT 0 query")
	}
}

func TestRequiredSchemaProbeValidatesMigration0011Shape(t *testing.T) {
	for _, required := range []string{
		"expected_search_columns",
		"site_search_documents",
		"site_search_queue",
		"site_search_index_status",
		"site_search_queries",
		"site_search_impressions",
		"site_search_clicks",
		"('site_search_documents', 'search_vector', 'tsvector', 'YES')",
		"('site_search_queue', 'lease_token', 'text', 'YES')",
		"('site_search_queue', 'locked_until', 'timestamptz', 'YES')",
		"('site_search_queue', 'last_error', 'text', 'YES')",
		"attribute_catalog.attgenerated = 's'",
		"attribute_catalog.atttypid = 'pg_catalog.tsvector'::pg_catalog.regtype",
		"pg_catalog.pg_get_expr",
		"LIKE '%''english''::regconfig%'",
		"LIKE '%setweight%'",
		"access_method.amname = 'gin'",
		"ARRAY['search_vector']::text[]",
		"ARRAY['site_id', 'version_number', 'page_path']::text[]",
		"ARRAY['impression_id', 'session_digest']::text[]",
		"ARRAY['available_at', 'updated_at', 'site_id']::text[]",
		"constraint_catalog.confdeltype = 'c'",
		"search_queue_has_no_foreign_key",
		"constraint_catalog.contype = 'f'",
		"search_queue_operation_ready",
		"LIKE '%reconcile%'",
		"LIKE '%delete%'",
	} {
		if !strings.Contains(requiredSchemaProbe, required) {
			t.Errorf("schema probe does not reference migration 0011 shape %q", required)
		}
	}

	for _, requiredReadyCheck := range []string{
		"search_columns_ready.ready",
		"search_vector_ready.ready",
		"search_vector_index_ready.ready",
		"search_keys_ready.ready",
		"search_indexes_ready.ready",
		"search_foreign_keys_ready.ready",
		"search_queue_has_no_foreign_key.ready",
		"search_queue_operation_ready.ready",
	} {
		if !strings.Contains(requiredSchemaProbe, requiredReadyCheck) {
			t.Errorf("schema probe does not combine %q", requiredReadyCheck)
		}
	}
}

func TestRequiredSchemaProbeValidatesMigration0012Shape(t *testing.T) {
	for _, required := range []string{
		"expected_collaboration_columns",
		"('versions', 'uploaded_by', 'uuid', 'YES', NULL)",
		"actual.column_default IS NOT DISTINCT FROM expected.column_default",
		"expected_collaboration_foreign_keys",
		"constraint_catalog.convalidated",
		"ARRAY['uploaded_by']::text[]",
		"constraint_catalog.confdeltype = expected.delete_action::\"char\"",
	} {
		if !strings.Contains(requiredSchemaProbe, required) {
			t.Errorf("schema probe does not reference migration 0012 shape %q", required)
		}
	}
	// Migration 0033 drops site_collaborators; probing for it would leave
	// every newer binary unready.
	if strings.Contains(requiredSchemaProbe, "site_collaborators") {
		t.Error("schema probe still requires site_collaborators")
	}

	for _, requiredReadyCheck := range []string{
		"collaboration_columns_ready.ready",
		"collaboration_foreign_keys_ready.ready",
	} {
		if !strings.Contains(requiredSchemaProbe, requiredReadyCheck) {
			t.Errorf("schema probe does not combine %q", requiredReadyCheck)
		}
	}
}
