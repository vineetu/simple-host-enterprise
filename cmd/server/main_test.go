package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/handler"
)

func newTestRedirectHandler(t *testing.T, publicBaseURL string) http.Handler {
	t.Helper()
	hosts, err := handler.NewHostModel(publicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := newHTTPSRedirectHandler(publicBaseURL, hosts)
	if err != nil {
		t.Fatal(err)
	}
	return redirect
}

func TestHTTPSRedirectUsesFixedAuthorityAndPreservesEscapedTarget(t *testing.T) {
	redirect := newTestRedirectHandler(t, "https://safe.example:8443")
	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/files/a%2Fb?q=x%2Fy&z=1", nil)
	request.Host = "attacker.example"
	request.Header.Set("X-Forwarded-Host", "also-attacker.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()

	redirect.ServeHTTP(response, request)

	if response.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", response.Code)
	}
	if got, want := response.Header().Get("Location"), "https://safe.example:8443/files/a%2Fb?q=x%2Fy&z=1"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	if strings.Contains(response.Header().Get("Location"), "attacker") {
		t.Fatal("redirect reflected request authority")
	}
}

func TestHTTPSRedirectIsHostAware(t *testing.T) {
	redirect := newTestRedirectHandler(t, "https://safe.example:8443")
	const path = "/files/a%2Fb?q=x%2Fy&z=1"
	cases := []struct {
		name          string
		host          string
		forwardedHost string
		want          string
	}{
		{name: "base host", host: "safe.example", want: "https://safe.example:8443" + path},
		{name: "base host with port", host: "safe.example:8080", want: "https://safe.example:8443" + path},
		{name: "owner host", host: "alice.safe.example", want: "https://alice.safe.example:8443" + path},
		{name: "owner host trailing dot and uppercase", host: "Alice.Safe.Example.", want: "https://alice.safe.example:8443" + path},
		{name: "owner host with port", host: "alice.safe.example:8080", want: "https://alice.safe.example:8443" + path},
		{name: "nested label", host: "a.b.safe.example", want: "https://safe.example:8443" + path},
		{name: "pod IP", host: "10.0.0.5:8080", want: "https://safe.example:8443" + path},
		{name: "foreign host", host: "evil.com", want: "https://safe.example:8443" + path},
		{name: "forwarded host is ignored", host: "safe.example", forwardedHost: "alice.safe.example", want: "https://safe.example:8443" + path},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+tc.host+path, nil)
			request.Host = tc.host
			if tc.forwardedHost != "" {
				request.Header.Set("X-Forwarded-Host", tc.forwardedHost)
				request.Header.Set("X-Forwarded-Proto", "https")
			}
			response := httptest.NewRecorder()

			redirect.ServeHTTP(response, request)

			if response.Code != http.StatusPermanentRedirect {
				t.Fatalf("status = %d, want 308", response.Code)
			}
			if got := response.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTTPSRedirectHandlerRejectsUnsafeBaseURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.com",
		"https://:443",
		"https://user@example.com",
		"https://example.com/subpath",
		"https://example.com?query=1",
		"https://example.com/#fragment",
		"https://example.com/#",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := newHTTPSRedirectHandler(raw, handler.HostModel{}); err == nil {
				t.Fatalf("newHTTPSRedirectHandler(%q) succeeded", raw)
			}
		})
	}
}

func TestServerTimeoutPolicies(t *testing.T) {
	if serverShutdownTimeout != 6*time.Minute {
		t.Fatalf("serverShutdownTimeout = %v, want 6m", serverShutdownTimeout)
	}
	if searchWorkerShutdownTimeout <= 0 {
		t.Fatalf("searchWorkerShutdownTimeout = %v, want a bounded positive duration", searchWorkerShutdownTimeout)
	}
	application := newApplicationServer(":0", http.NotFoundHandler())
	if application.ReadHeaderTimeout != applicationReadHeaderTimeout ||
		application.ReadTimeout != applicationReadTimeout ||
		application.IdleTimeout != applicationIdleTimeout ||
		application.MaxHeaderBytes != applicationMaxHeaderBytes {
		t.Fatalf("application server timeout policy = %+v", application)
	}
	if application.WriteTimeout != 0 {
		t.Fatalf("application WriteTimeout = %v, want 0 so five-minute streams are handler-bounded", application.WriteTimeout)
	}

	redirect := newRedirectServer(":0", http.NotFoundHandler())
	if redirect.ReadHeaderTimeout <= 0 || redirect.ReadTimeout <= 0 ||
		redirect.WriteTimeout <= 0 || redirect.IdleTimeout <= 0 ||
		redirect.MaxHeaderBytes <= 0 {
		t.Fatalf("redirect server has unbounded policy: %+v", redirect)
	}
}

func TestPublicSearchHandlerConstructionAndRouteRegistration(t *testing.T) {
	if _, err := newPublicSearchHandler(nil, handler.CookiePolicy{}, handler.NewAbuseLimits()); err == nil {
		t.Fatal("nil database unexpectedly constructed a public search handler")
	}

	searchHandler, err := newPublicSearchHandler(
		new(sql.DB),
		handler.CookiePolicy{Secure: true},
		handler.NewAbuseLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// This test is about route registration and request-body handling, not
	// the session requirement design.md 7.2 added around search, so a
	// pass-through stands in for the real auth middleware.
	searchHandler.Register(mux, func(next http.Handler) http.Handler { return next })

	getRequest := httptest.NewRequest(http.MethodGet, "/api/search", nil)
	getRequest.RemoteAddr = "192.0.2.60:1234"
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusBadRequest || getResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET route = status %d, Cache-Control %q, body %s", getResponse.Code, getResponse.Header().Get("Cache-Control"), getResponse.Body)
	}
	if cookies := getResponse.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("invalid search unexpectedly set cookies: %+v", cookies)
	}

	clickRequest := httptest.NewRequest(http.MethodPost, "/api/search/click", strings.NewReader(`{}`))
	clickRequest.RemoteAddr = "192.0.2.61:1234"
	clickResponse := httptest.NewRecorder()
	mux.ServeHTTP(clickResponse, clickRequest)
	if clickResponse.Code != http.StatusBadRequest || clickResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("click route = status %d, Cache-Control %q, body %s", clickResponse.Code, clickResponse.Header().Get("Cache-Control"), clickResponse.Body)
	}

	wrongMethod := httptest.NewRecorder()
	mux.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPut, "/api/search", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/search status = %d, want 405", wrongMethod.Code)
	}
}

func TestApplicationResourcesStopsAndJoinsAllWorkersBeforeClosingDependencies(t *testing.T) {
	var events []string
	resources := applicationResources{
		workers: []workerLifecycle{
			&recordingLifecycleWorker{
				name:   "indexer",
				events: &events,
				wait: func(context.Context) error {
					return nil
				},
			},
			&recordingLifecycleWorker{
				name:   "pruner",
				events: &events,
				wait: func(context.Context) error {
					return nil
				},
			},
		},
		store:                  recordingCloser{name: "store", events: &events},
		database:               recordingCloser{name: "database", events: &events},
		workersShutdownTimeout: time.Second,
	}

	if err := resources.close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"indexer-stop", "pruner-stop", "indexer-wait", "pruner-wait", "store-close", "database-close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle order = %v, want %v", events, want)
	}
}

func TestApplicationResourcesRefusesToCloseDependenciesBeforeJoin(t *testing.T) {
	var events []string
	resources := applicationResources{
		workers: []workerLifecycle{
			&recordingLifecycleWorker{
				name:   "indexer",
				events: &events,
				wait: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			},
			&recordingLifecycleWorker{
				name:   "pruner",
				events: &events,
				wait: func(ctx context.Context) error {
					return ctx.Err()
				},
			},
		},
		store:                  recordingCloser{name: "store", events: &events},
		database:               recordingCloser{name: "database", events: &events},
		workersShutdownTimeout: 10 * time.Millisecond,
	}

	started := time.Now()
	err := resources.close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("bounded worker join took %v", elapsed)
	}
	want := []string{"indexer-stop", "pruner-stop", "indexer-wait", "pruner-wait"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle order = %v, want %v", events, want)
	}
}

func TestRunServersCoordinatesShutdown(t *testing.T) {
	listeners := make([]net.Listener, 2)
	servers := make([]*http.Server, 2)
	managed := make([]managedServer, 2)
	for i := range listeners {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = listener
		server := newApplicationServer(listener.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		servers[i] = server
		managed[i] = managedServer{
			name:   listener.Addr().String(),
			server: server,
			serve: func() error {
				return server.Serve(listener)
			},
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runServers(ctx, time.Second, managed...)
	}()

	client := &http.Client{Timeout: time.Second}
	for _, listener := range listeners {
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			cancel()
			t.Fatalf("listener %s was not serving: %v", listener.Addr(), err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			cancel()
			t.Fatalf("listener %s status = %d", listener.Addr(), response.StatusCode)
		}
	}

	shutdownStarted := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServers: %v", err)
		}
		if elapsed := time.Since(shutdownStarted); elapsed >= time.Second {
			t.Fatalf("idle shutdown took %v; it waited toward the configured timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runServers did not coordinate listener shutdown")
	}

	for _, listener := range listeners {
		connection, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			t.Fatalf("listener %s still accepts connections after shutdown", listener.Addr())
		}
	}
}

func TestRunServersDrainsActiveHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	server := newApplicationServer(listener.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runServers(ctx, 2*time.Second, managedServer{
			name:   "application",
			server: server,
			serve: func() error {
				return server.Serve(listener)
			},
		})
	}()

	requestDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("status = %d", response.StatusCode)
			}
		}
		requestDone <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("runServers returned before active handler drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("active request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServers: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServers did not return after active handler finished")
	}
}

func TestRunServersReportsListenerFailureAndStopsPeer(t *testing.T) {
	failedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := failedListener.Close(); err != nil {
		t.Fatal(err)
	}
	peerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	failedServer := newRedirectServer(failedListener.Addr().String(), http.NotFoundHandler())
	peerServer := newApplicationServer(peerListener.Addr().String(), http.NotFoundHandler())
	got := runServers(context.Background(), time.Second,
		managedServer{
			name:   "failed",
			server: failedServer,
			serve: func() error {
				return failedServer.Serve(failedListener)
			},
		},
		managedServer{
			name:   "peer",
			server: peerServer,
			serve: func() error {
				return peerServer.Serve(peerListener)
			},
		},
	)
	if got == nil || !strings.Contains(got.Error(), "failed listener") {
		t.Fatalf("runServers error = %v, want failed-listener error", got)
	}
	if !errors.Is(got, net.ErrClosed) {
		t.Fatalf("runServers error = %v, want wrapped net.ErrClosed", got)
	}
	connection, dialErr := net.DialTimeout("tcp", peerListener.Addr().String(), 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatal("peer listener remained open after other listener failed")
	}
}

func TestRunServersReportsEveryUnexpectedListenerExit(t *testing.T) {
	for _, test := range []struct {
		name     string
		serveErr error
	}{
		{name: "nil", serveErr: nil},
		{name: "server closed", serveErr: http.ErrServerClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newApplicationServer(":0", http.NotFoundHandler())
			err := runServers(context.Background(), time.Second, managedServer{
				name:   "application",
				server: server,
				serve: func() error {
					return test.serveErr
				},
			})
			if err == nil || !strings.Contains(err.Error(), "application listener") {
				t.Fatalf("runServers error = %v, want unexpected listener-exit error", err)
			}
		})
	}
}

func TestRunServersCancelsBackgroundWorkWhenListenerFails(t *testing.T) {
	var events []string
	resources := applicationResources{workers: []workerLifecycle{
		&recordingLifecycleWorker{name: "indexer", events: &events},
		&recordingLifecycleWorker{name: "pruner", events: &events},
	}}
	server := newApplicationServer(":0", http.NotFoundHandler())
	err := runServersWithShutdownHook(
		context.Background(),
		time.Second,
		resources.stopWorkers,
		managedServer{
			name:   "application",
			server: server,
			serve: func() error {
				return errors.New("forced listener failure")
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "forced listener failure") {
		t.Fatalf("runServersWithShutdownHook error = %v", err)
	}
	if want := []string{"indexer-stop", "pruner-stop"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("listener failure background shutdown = %v, want %v", events, want)
	}
}

type recordingLifecycleWorker struct {
	name   string
	events *[]string
	wait   func(context.Context) error
}

func (w *recordingLifecycleWorker) Stop() {
	*w.events = append(*w.events, w.name+"-stop")
}

func (w *recordingLifecycleWorker) Wait(ctx context.Context) error {
	*w.events = append(*w.events, w.name+"-wait")
	if w.wait == nil {
		return nil
	}
	return w.wait(ctx)
}

type recordingCloser struct {
	name   string
	events *[]string
}

func (c recordingCloser) Close() error {
	*c.events = append(*c.events, c.name+"-close")
	return nil
}
