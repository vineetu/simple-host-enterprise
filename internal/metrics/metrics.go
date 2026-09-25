// Package metrics serves a small Prometheus text-format endpoint: request
// counts and latency, the database pool, and build info. It is written by
// hand rather than with the Prometheus client library because these few
// series are all the package needs, and the endpoint listens on its own port
// that the Service and Ingress never expose.
package metrics

import (
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// buckets are the latency histogram's upper bounds, in seconds.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// Registry holds the request counters. The zero value is not usable; call New.
type Registry struct {
	mu      sync.Mutex
	byClass [6]uint64 // index 1..5 is the status class; 0 is anything else
	counts  []uint64  // cumulative per bucket, then +Inf
	sum     float64
	total   uint64
}

func New() *Registry {
	return &Registry{counts: make([]uint64, len(buckets)+1)}
}

func (r *Registry) observe(status int, elapsed time.Duration) {
	seconds := elapsed.Seconds()
	class := status / 100
	if class < 1 || class > 5 {
		class = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byClass[class]++
	r.total++
	r.sum += seconds
	for i, bound := range buckets {
		if seconds <= bound {
			r.counts[i]++
		}
	}
	r.counts[len(buckets)]++
}

// Middleware counts every request the wrapped handler answers.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		started := time.Now()
		defer func() {
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			r.observe(status, time.Since(started))
		}()
		next.ServeHTTP(recorder, req)
	})
}

// Build describes the running binary for simplehost_build_info.
type Build struct {
	Version, Commit, Schema string
}

// Handler writes the exposition. db may be nil.
func (r *Registry) Handler(db *sql.DB, build Build) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintln(w, "# HELP simplehost_build_info The running binary.")
		fmt.Fprintln(w, "# TYPE simplehost_build_info gauge")
		fmt.Fprintf(w, "simplehost_build_info{version=%q,commit=%q,schema=%q} 1\n", build.Version, build.Commit, build.Schema)

		r.mu.Lock()
		byClass := r.byClass
		counts := append([]uint64(nil), r.counts...)
		sum, total := r.sum, r.total
		r.mu.Unlock()

		fmt.Fprintln(w, "# HELP simplehost_http_requests_total HTTP requests answered, by status class.")
		fmt.Fprintln(w, "# TYPE simplehost_http_requests_total counter")
		for class := 1; class <= 5; class++ {
			fmt.Fprintf(w, "simplehost_http_requests_total{code=\"%dxx\"} %d\n", class, byClass[class])
		}
		if byClass[0] > 0 {
			fmt.Fprintf(w, "simplehost_http_requests_total{code=\"other\"} %d\n", byClass[0])
		}

		fmt.Fprintln(w, "# HELP simplehost_http_request_duration_seconds Time to answer an HTTP request.")
		fmt.Fprintln(w, "# TYPE simplehost_http_request_duration_seconds histogram")
		for i, bound := range buckets {
			fmt.Fprintf(w, "simplehost_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", bound, counts[i])
		}
		fmt.Fprintf(w, "simplehost_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", counts[len(buckets)])
		fmt.Fprintf(w, "simplehost_http_request_duration_seconds_sum %g\n", sum)
		fmt.Fprintf(w, "simplehost_http_request_duration_seconds_count %d\n", total)

		if db == nil {
			return
		}
		stats := db.Stats()
		fmt.Fprintln(w, "# HELP simplehost_db_connections_open Open database connections.")
		fmt.Fprintln(w, "# TYPE simplehost_db_connections_open gauge")
		fmt.Fprintf(w, "simplehost_db_connections_open %d\n", stats.OpenConnections)
		fmt.Fprintln(w, "# HELP simplehost_db_connections_in_use Database connections in use.")
		fmt.Fprintln(w, "# TYPE simplehost_db_connections_in_use gauge")
		fmt.Fprintf(w, "simplehost_db_connections_in_use %d\n", stats.InUse)
		fmt.Fprintln(w, "# HELP simplehost_db_wait_count_total Times a query waited for a free connection.")
		fmt.Fprintln(w, "# TYPE simplehost_db_wait_count_total counter")
		fmt.Fprintf(w, "simplehost_db_wait_count_total %d\n", stats.WaitCount)
		fmt.Fprintln(w, "# HELP simplehost_db_wait_seconds_total Time spent waiting for a free connection.")
		fmt.Fprintln(w, "# TYPE simplehost_db_wait_seconds_total counter")
		fmt.Fprintf(w, "simplehost_db_wait_seconds_total %g\n", stats.WaitDuration.Seconds())
	})
}

// statusRecorder captures the status without changing how the handler
// writes. Unwrap keeps http.ResponseController working, and Flush keeps the
// streaming handlers streaming.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
