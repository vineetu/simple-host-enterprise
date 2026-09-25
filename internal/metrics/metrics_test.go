package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareCountsByStatusClass(t *testing.T) {
	registry := New()
	app := registry.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	for _, path := range []string{"/", "/", "/missing"} {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	response := httptest.NewRecorder()
	registry.Handler(nil, Build{Version: "v1.2.3", Commit: "abc", Schema: "0028"}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`simplehost_http_requests_total{code="2xx"} 2`,
		`simplehost_http_requests_total{code="4xx"} 1`,
		`simplehost_http_request_duration_seconds_count 3`,
		`simplehost_http_request_duration_seconds_bucket{le="+Inf"} 3`,
		`simplehost_build_info{version="v1.2.3",commit="abc",schema="0028"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
