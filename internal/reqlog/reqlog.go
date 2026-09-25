// Package reqlog writes one structured line per HTTP request and gives every
// request an id. The line is what a SIEM ingests from the pod's stdout, and
// the id is what ties that line to the audit rows the same request wrote and
// to the error a user reports, so it is generated here, never taken from the
// caller: an inbound X-Request-Id is somebody else's identifier.
package reqlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Header carries the request id back to the client on every response.
const Header = "X-Request-Id"

type contextKey struct{}

// Record is the per-request state the middleware keeps. Inner middleware that
// learns who the caller is stores it here so the log line can carry it even
// though the line is written outside their scope.
type Record struct {
	ID string
	// IP is ClientIP of the request and UserAgent its User-Agent, kept here
	// so code that has only the context (the audit recorder) can fill them.
	IP        string
	UserAgent string

	mu     sync.Mutex
	userID string
}

func (r *Record) setUser(id string) {
	r.mu.Lock()
	r.userID = id
	r.mu.Unlock()
}

func (r *Record) user() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.userID
}

// FromContext returns the request's record, or nil outside the middleware.
func FromContext(ctx context.Context) *Record {
	record, _ := ctx.Value(contextKey{}).(*Record)
	return record
}

// SetUser records the authenticated principal for the request's log line. It
// is a no-op outside the middleware so handlers can call it unconditionally.
func SetUser(ctx context.Context, userID string) {
	if record := FromContext(ctx); record != nil {
		record.setUser(userID)
	}
}

// Middleware assigns the id, echoes it, and logs the request once the handler
// returns. skip decides which requests are not logged; the probes are the
// usual case, because a kubelet asking twice a second is noise with no reader.
// A skipped request that failed is logged anyway: a failing probe is the one
// probe somebody needs to see.
func Middleware(logger *slog.Logger, skip func(*http.Request) bool) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record := &Record{ID: newID(), IP: ClientIP(r), UserAgent: r.UserAgent()}
			w.Header().Set(Header, record.ID)
			recorder := &statusRecorder{ResponseWriter: w}
			started := time.Now()
			ctx := context.WithValue(r.Context(), contextKey{}, record)

			defer func() {
				status := recorder.status
				if status == 0 {
					status = http.StatusOK
				}
				if skip != nil && skip(r) && status < 400 {
					return
				}
				attrs := []slog.Attr{
					slog.String("request_id", record.ID),
					slog.String("method", r.Method),
					slog.String("host", r.Host),
					slog.String("path", r.URL.Path),
					slog.Int("status", status),
					slog.Int64("bytes", recorder.bytes),
					slog.Int64("duration_ms", time.Since(started).Milliseconds()),
					slog.String("remote", record.IP),
					slog.String("user_agent", r.UserAgent()),
				}
				if user := record.user(); user != "" {
					attrs = append(attrs, slog.String("user_id", user))
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
			}()

			next.ServeHTTP(recorder, r.WithContext(ctx))
		})
	}
}

// ProbePaths reports whether the request is a Kubernetes probe.
func ProbePaths(r *http.Request) bool {
	return r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// rand.Read failing means the process has bigger problems; a
		// time-derived id still lets the line be found.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102T150405.000000")))[:32]
	}
	return hex.EncodeToString(raw[:])
}

var trustedProxies atomic.Pointer[[]netip.Prefix]

// SetTrustedProxies installs TRUSTED_PROXY_CIDRS. Call once at startup,
// before serving.
func SetTrustedProxies(prefixes []netip.Prefix) {
	trustedProxies.Store(&prefixes)
}

func isTrustedProxy(addr netip.Addr) bool {
	prefixes := trustedProxies.Load()
	if prefixes == nil {
		return false
	}
	for _, p := range *prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP is the one answer to "which address did this request come
// from", used by rate limits, the request log, access_log, sessions.ip and
// audit rows alike. It is the TCP peer, unless the peer is inside
// TRUSTED_PROXY_CIDRS: then X-Forwarded-For is read right to left and the
// first address that is not itself a trusted proxy is the client. Entries
// further left were written by the client and are never believed. "" when
// the peer address cannot be parsed.
func ClientIP(r *http.Request) string {
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok {
		return ""
	}
	if !isTrustedProxy(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseAddr(strings.TrimSpace(hops[i]))
		if !ok {
			break
		}
		client = addr
		if !isTrustedProxy(addr) {
			break
		}
	}
	return client.String()
}

func parseAddr(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// statusRecorder captures what the handler wrote without changing how it
// wrote it. Unwrap keeps http.ResponseController working, and Flush keeps the
// streaming handlers streaming.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
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
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
