package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// APIErrors keeps unmatched API requests out of the website catch-alls. Looking
// up patterns before serving also catches method mismatches hidden by GET / or
// POST /, without buffering downloads or changing a route's own error response.
func APIErrors(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		if !apiFallbackPattern(pattern) {
			if isMCPPath(r.URL.Path) {
				rec := &mcpErrorWriter{ResponseWriter: w}
				next.ServeHTTP(rec, r)
				rec.finish()
			} else {
				next.ServeHTTP(w, r)
			}
			return
		}

		// These are the methods used by this service. Probe only route lookup,
		// never handlers, and exclude the website catch-alls from Allow.
		var allowed []string
		probe := r.Clone(r.Context())
		for _, method := range []string{"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"} {
			probe.Method = method
			_, pattern := mux.Handler(probe)
			if !apiFallbackPattern(pattern) {
				allowed = append(allowed, method)
			}
		}
		status, code, message := http.StatusNotFound, "not_found", "API route not found"
		if len(allowed) != 0 {
			status, code, message = http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
			if r.URL.Path == "/mcp" {
				allowed = []string{"POST"}
			}
			w.Header().Set("Allow", strings.Join(allowed, ", "))
		}
		if isMCPPath(r.URL.Path) {
			writeMCPError(w, status, message)
			return
		}
		writeJSON(w, status, errorResponse{
			Error: message, Code: code,
			Hint: "See /openapi.yaml for supported routes and methods.",
		})
	})
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/") || path == "/mcp" || strings.HasPrefix(path, "/mcp/")
}

func apiFallbackPattern(pattern string) bool {
	return pattern == "" || pattern == "/" || pattern == "GET /" || pattern == "POST /"
}

func isMCPPath(path string) bool { return path == "/mcp" || strings.HasPrefix(path, "/mcp/") }

func writeMCPError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": message}})
}

// Only transport failures are buffered. Successful streams pass through.
type mcpErrorWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *mcpErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mcpErrorWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status < 400 {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *mcpErrorWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.status >= 400 {
		return w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}
func (w *mcpErrorWriter) finish() {
	if w.status < 400 {
		return
	}
	w.Header().Del("Content-Length")
	var value map[string]json.RawMessage
	if json.Unmarshal(w.body.Bytes(), &value) == nil && string(value["jsonrpc"]) == `"2.0"` {
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.body.Bytes())
		return
	}
	message := http.StatusText(w.status)
	if raw := value["error"]; raw != nil {
		_ = json.Unmarshal(raw, &message)
	}
	writeMCPError(w.ResponseWriter, w.status, message)
}
