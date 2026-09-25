package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/config"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/mcp"
	"github.com/vsriram/simple-host/internal/migrate"
	"github.com/vsriram/simple-host/internal/oidc"
	"github.com/vsriram/simple-host/internal/reqlog"
	"github.com/vsriram/simple-host/internal/search"
	"github.com/vsriram/simple-host/internal/sitetype"
	"github.com/vsriram/simple-host/internal/storage"
)

const (
	applicationReadHeaderTimeout = 5 * time.Second
	applicationReadTimeout       = 5 * time.Minute
	// Streaming handlers own their five-minute context. A server-wide write
	// deadline starts before request-body processing and would truncate valid
	// streams, so it is deliberately disabled here.
	applicationWriteTimeout   = 0
	applicationIdleTimeout    = 60 * time.Second
	applicationMaxHeaderBytes = 1 << 20

	redirectReadHeaderTimeout = 5 * time.Second
	redirectReadTimeout       = 10 * time.Second
	redirectWriteTimeout      = 10 * time.Second
	redirectIdleTimeout       = 30 * time.Second
	redirectMaxHeaderBytes    = 64 << 10

	// Active handlers can legitimately use the five-minute application
	// request/stream window. Leave a bounded minute for post-read publication
	// and reconciliation. http.Server.Shutdown still returns immediately when
	// there is nothing in flight.
	serverShutdownTimeout = 6 * time.Minute

	searchWorkerShutdownTimeout = 10 * time.Second
)

func main() {
	if len(os.Args) > 1 {
		if err := runSubcommand(os.Args[1], os.Args[2:]); err != nil {
			log.Printf("%s: %v", os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	database.SetMaxOpenConns(maxOpenDBConns)
	database.SetMaxIdleConns(maxOpenDBConns / 2)
	database.SetConnMaxIdleTime(5 * time.Minute)
	// The schema gate: this binary serves only the schema it embeds. Behind
	// means `simple-host migrate` did not run; ahead means a rollback landed
	// on a contracted schema, and failing here beats failing on a query.
	if err := migrate.Check(context.Background(), database); err != nil {
		database.Close()
		return fmt.Errorf("schema check: %w", err)
	}
	resources := applicationResources{
		database:               database,
		workersShutdownTimeout: searchWorkerShutdownTimeout,
	}
	defer func() {
		runErr = errors.Join(runErr, resources.close())
	}()

	siteStore, err := openStore(cfg, database)
	if err != nil {
		return err
	}
	resources.store = siteStore

	mux := http.NewServeMux()
	hosts, err := handler.NewHostModel(cfg.PublicBaseURL)
	if err != nil {
		return fmt.Errorf("derive host model from public base URL: %w", err)
	}
	handler.SetExtraReservedLabels(cfg.ReservedLabels)
	cookiePolicy := handler.CookiePolicy{Secure: cfg.SecureMode}
	abuseLimits := handler.NewAbuseLimits()
	publicSearchHandler, err := newPublicSearchHandler(database, cookiePolicy, abuseLimits)
	if err != nil {
		return fmt.Errorf("create public search handler: %w", err)
	}

	signingKeys := toAuthSigningKeys(cfg.Session.SigningKeys)
	log.Printf("session cookie: %s", auth.DescribeSigningKeys(signingKeys))
	authMW := auth.Middleware(database, signingKeys, cfg.Session.Idle)
	// auditRecorder persists audit_events (design.md 8.1); accessWriter
	// batches access_log (design.md 8.2). Both are real, database-backed
	// sinks as of Phase 4 — see docs/security-review.md.
	// accessWriter is closed in applicationResources.close(), which runs
	// only after runServersWithShutdownHook's http.Server.Shutdown calls
	// have returned, per its own doc comment's ordering requirement.
	auditRecorder := audit.NewDBRecorder(database)
	accessWriter := audit.NewAccessWriter(database)
	resources.accessWriter = accessWriter

	oidcProvider, err := oidc.Discover(context.Background(), oidc.Config{
		Issuer:       cfg.OIDC.Issuer,
		ClientID:     cfg.OIDC.ClientID,
		ClientSecret: cfg.OIDC.ClientSecret,
		RedirectURL:  strings.TrimRight(cfg.PublicBaseURL, "/") + "/auth/callback",
		Scopes:       cfg.OIDC.Scopes,
	}, nil)
	if err != nil {
		return fmt.Errorf("discover OIDC provider: %w", err)
	}
	oidcClaims := handler.OIDCClaimConfig{
		EmailClaim:          cfg.OIDC.EmailClaim,
		UsernameClaim:       cfg.OIDC.UsernameClaim,
		AdminClaim:          cfg.OIDC.AdminClaim,
		AdminValue:          cfg.OIDC.AdminValue,
		AdminEmails:         cfg.OIDC.AdminEmails,
		AllowedEmailDomains: cfg.OIDC.AllowedEmailDomains,
		HintDomain:          cfg.OIDC.HintDomain,
	}

	// Management-client version middleware. It prevents versionless legacy
	// agents from using obsolete site-management semantics, publishes update
	// metadata as headers without rewriting response bodies, and remains safe
	// for streaming archive responses. PFB, state, and public/static serving do
	// not receive it.
	pluginVersion, err := handler.PluginVersion()
	if err != nil {
		return fmt.Errorf("read plugin.json version: %w", err)
	}
	skillVersionMW, err := handler.SkillVersionMiddleware(pluginVersion, handler.MinimumSupportedSkillVersion)
	if err != nil {
		return fmt.Errorf("configure skill version middleware: %w", err)
	}
	log.Printf("simple-host skill version: %s", pluginVersion)

	handler.RegisterHealthRoutes(mux, database, siteStore.Ping)
	publicSearchHandler.Register(mux, authMW)
	handler.NewUserHandler(database, abuseLimits).Register(mux, authMW, skillVersionMW)
	handler.NewSiteHandler(database, siteStore, cfg.PublicBaseURL, hosts, abuseLimits).WithAudit(auditRecorder).Register(mux, authMW, skillVersionMW)
	handler.NewTeamHandler(database, abuseLimits).WithAudit(auditRecorder).Register(mux, authMW, skillVersionMW, hosts, cfg.PublicBaseURL)
	// Held rather than registered inline: the classification worker starts
	// after the routes are wired, and the handler is given it once it exists.
	// The route closures capture this pointer, so attaching later is enough.
	auditReader := audit.NewReader(database)
	adminHandler := handler.NewAdminHandler(database, cfg.PublicBaseURL, hosts, cookiePolicy, signingKeys, cfg.Session.Idle, auditRecorder, abuseLimits).WithStore(siteStore).WithAuditReader(auditReader)
	adminHandler.Register(mux, authMW, skillVersionMW)
	handler.NewAuditHandler(database, auditReader, cfg.Audit.AccessLogVisibility, abuseLimits).Register(mux, authMW, skillVersionMW)
	handler.NewShowcaseHandler(database, hosts, signingKeys, cfg.Session.Idle).Register(mux)
	handler.NewAuthHandler(database, oidcProvider, oidcClaims, signingKeys, cfg.Session.TTL, cfg.Session.Idle, auditRecorder, hosts, cfg.PublicBaseURL, abuseLimits).Register(mux, authMW)
	handler.NewKeysHandler(database, auditRecorder, hosts, cfg.PublicBaseURL, abuseLimits).Register(mux, authMW)
	handler.NewDashboardHandler(database, signingKeys, cfg.Session.Idle).Register(mux, authMW)
	handoffHandler := handler.NewHandoffHandler(database, signingKeys, hosts, auditRecorder, abuseLimits)
	handoffHandler.Register(mux, authMW)
	handler.RegisterUIRoutes(mux)
	siteFiles := handler.NewSiteFiles(siteStore, database, cookiePolicy, signingKeys, cfg.Session.Idle).WithAccessWriter(accessWriter)
	siteAPIHandler := handler.NewSiteAPIHandler(database, siteStore, storage.AssetLimits{
		MaxFileBytes: cfg.Assets.MaxFileBytes,
		MaxSiteBytes: cfg.Assets.MaxSiteBytes,
		MaxSiteCount: cfg.Assets.MaxSiteCount,
	}, auditRecorder, hosts, abuseLimits)

	// The negative session cache is hosted content's substitute for a
	// per-request read of the sessions table (design.md 5.1, 6.1): it
	// refreshes every 60 seconds and the host gate consults the in-memory
	// snapshot instead. Started after every other dependency exists and
	// stopped before the database connection closes.
	negativeSessionCache := auth.NewNegativeSessionCache(database, cfg.Session.Idle)
	negativeSessionCache.Start()
	resources.sessionCache = negativeSessionCache

	// MCP is mounted last and given the finished mux: every tool is served back
	// into the same router, so the protocol is an adapter over the REST surface
	// rather than a second implementation of it.
	// Registered per method rather than as a bare "/mcp": a pattern with no
	// method conflicts with the UI's "GET /" catch-all, and GET and DELETE must
	// answer 405 themselves so an older client can detect the era instead of
	// being handed the landing page.
	mcpServer := mcp.NewServer(mux, "simple-host", pluginVersion)
	mux.Handle("POST /mcp", mcpServer)
	mux.Handle("GET /mcp", mcpServer)
	mux.Handle("DELETE /mcp", mcpServer)

	// The host gate sits directly around the mux: it decides, per hostname,
	// which routes the mux may answer. The base host is control plane only
	// and refuses the site-facing API shape outright; an owner host
	// ("<label>.<base>") serves only that owner's hosted content, its
	// site-facing API, and the session hand-off, all behind a valid host
	// session; a restricted site serves the same three things on its own
	// "<owner>--<site>.<base>" host instead. Anything else gets probes only.
	// The request log wraps everything, so every response carries a request
	// id and every request, including one the gate refuses, is on record.
	requestLog := reqlog.Middleware(slog.New(slog.NewJSONHandler(os.Stdout, nil)), reqlog.ProbePaths)
	hostGate := handler.NewHostGate(hosts, siteFiles, database, signingKeys, negativeSessionCache, handoffHandler, siteAPIHandler, authMW, cfg.PublicBaseURL)
	applicationServer := newApplicationServer(":"+cfg.Port, requestLog(handler.SecurityHeaders(hostGate(mux), cfg.SecureMode, hosts)))
	servers := []managedServer{manageHTTPServer("application", applicationServer)}
	if cfg.SecureMode {
		redirectHandler, err := newHTTPSRedirectHandler(cfg.PublicBaseURL, hosts)
		if err != nil {
			return fmt.Errorf("create HTTPS redirect handler: %w", err)
		}
		servers = append(servers, manageHTTPServer(
			"HTTPS redirect",
			newRedirectServer(":"+cfg.RedirectPort, redirectHandler),
		))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Indexed pages carry whatever address SiteLink gives, so the index
	// follows the cutover; existing documents are reindexed by hand after the
	// flip (docs/subdomains/migration.md phase 4).
	searchWorker, err := search.StartWorker(ctx, database, searchVersions{siteStore}, hosts.SiteLink)
	if err != nil {
		return fmt.Errorf("start site search worker: %w", err)
	}
	resources.workers = append(resources.workers, searchWorker)
	telemetryPruner, err := search.StartTelemetryPruner(ctx, database)
	if err != nil {
		return fmt.Errorf("start site search telemetry pruner: %w", err)
	}
	resources.workers = append(resources.workers, telemetryPruner)
	resources.workers = append(resources.workers, startLoop(ctx, func(ctx context.Context) {
		siteStore.RunSweeper(ctx, database)
	}))

	// Site-type classification has no classifier in this package: the showcase
	// shows no type chips, and the worker is not started. An installer with a
	// model can supply one through sitetype.StartWorker.
	_ = sitetype.StartWorker

	for _, server := range servers {
		log.Printf("%s listening on %s", server.name, server.server.Addr)
	}
	if err := runServersWithShutdownHook(ctx, serverShutdownTimeout, resources.stopWorkers, servers...); err != nil {
		return err
	}
	return nil
}

// maxOpenDBConns caps each replica's pool, so replicas times this stays well
// inside a typical Postgres max_connections.
const maxOpenDBConns = 20

// openStore builds the site store the server and the storage subcommands
// share: the configured bucket, the database as the index of what is live,
// and the pod-local cache.
func openStore(cfg config.Config, database *sql.DB) (*storage.Store, error) {
	objects, err := storage.NewS3Objects(context.Background(), s3Config(cfg))
	if err != nil {
		return nil, fmt.Errorf("create bucket client: %w", err)
	}
	store, err := storage.New(storage.Options{
		Objects:       objects,
		Index:         storage.NewDBIndex(database),
		CacheDir:      cfg.CacheDir,
		CacheMaxBytes: cfg.CacheMaxBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("create site store: %w", err)
	}
	return store, nil
}

func s3Config(cfg config.Config) storage.S3Config {
	return storage.S3Config{
		Endpoint:        cfg.Backup.Endpoint,
		Region:          cfg.Backup.Region,
		Bucket:          cfg.Backup.Bucket,
		Prefix:          cfg.Backup.Prefix,
		AccessKeyID:     cfg.Backup.AccessKeyID,
		SecretAccessKey: cfg.Backup.SecretAccessKey,
		SSE:             cfg.Backup.SSE,
		SSEKMSKeyID:     cfg.Backup.SSEKMSKeyID,
		EnvelopeKeys:    toStorageEnvelopeKeys(cfg.Backup.EnvelopeKeys),
	}
}

// searchVersions adapts the store to the search worker's VersionOpener.
type searchVersions struct{ store *storage.Store }

func (s searchVersions) OpenVersion(ctx context.Context, siteID string, version int) (search.OpenedVersion, error) {
	lease, err := s.store.OpenVersion(ctx, siteID, version)
	if err != nil {
		return nil, err
	}
	return lease, nil
}

// loop is a background goroutine with the worker lifecycle.
type loop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startLoop(parent context.Context, run func(context.Context)) *loop {
	ctx, cancel := context.WithCancel(parent)
	l := &loop{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		run(ctx)
	}()
	return l
}

func (l *loop) Stop() { l.cancel() }

func (l *loop) Wait(ctx context.Context) error {
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// toStorageEnvelopeKeys copies config's envelope-key list into storage's own
// type. Both packages define the same small struct rather than one importing
// the other; see the comment on storage.S3Config.
func toStorageEnvelopeKeys(keys []config.EnvelopeKey) []storage.EnvelopeKey {
	if len(keys) == 0 {
		return nil
	}
	out := make([]storage.EnvelopeKey, len(keys))
	for i, k := range keys {
		out[i] = storage.EnvelopeKey{ID: k.ID, Key: k.Key}
	}
	return out
}

// toAuthSigningKeys copies config's session signing keys into auth's own
// type, the same one-file-per-package-boundary shape toStorageEnvelopeKeys
// uses above.
func toAuthSigningKeys(keys []config.SigningKey) []auth.SigningKey {
	if len(keys) == 0 {
		return nil
	}
	out := make([]auth.SigningKey, len(keys))
	for i, k := range keys {
		out[i] = auth.SigningKey{ID: k.ID, Key: k.Key}
	}
	return out
}

func newPublicSearchHandler(
	database *sql.DB,
	cookiePolicy handler.CookiePolicy,
	abuseLimits *handler.AbuseLimits,
) (*handler.SearchHandler, error) {
	backend, err := search.NewPostgreSQLPublicBackend(database)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL public search backend: %w", err)
	}
	service, err := search.NewPublicService(backend)
	if err != nil {
		return nil, fmt.Errorf("create public search service: %w", err)
	}
	return handler.NewSearchHandler(database, service, cookiePolicy, abuseLimits), nil
}

type workerLifecycle interface {
	Stop()
	Wait(context.Context) error
}

type applicationResources struct {
	workers                []workerLifecycle
	store                  io.Closer
	database               io.Closer
	workersShutdownTimeout time.Duration
	// sessionCache is the hosted-content negative cache's background refresh
	// loop. Stopped alongside the other workers, before the database
	// connection closes.
	sessionCache *auth.NegativeSessionCache
	// accessWriter batches access_log inserts (design.md 8.2). Closed here,
	// after every worker has stopped and after runServersWithShutdownHook's
	// http.Server.Shutdown calls have already returned (close runs from a
	// defer registered before that call, so it always runs after run()
	// returns) — audit.AccessWriter's own doc comment requires this
	// ordering: closing any earlier could drop a visit a still-draining
	// request was about to enqueue.
	accessWriter *audit.AccessWriter
}

func (r *applicationResources) stopWorkers() {
	for _, worker := range r.workers {
		if worker != nil {
			worker.Stop()
		}
	}
	if r.sessionCache != nil {
		r.sessionCache.Stop()
	}
}

// close preserves the dependency lifetime: all workers are canceled before any
// join begins, then every worker is joined before either storage or Postgres is
// closed, regardless of whether shutdown began from a signal or listener exit.
func (r *applicationResources) close() error {
	r.stopWorkers()
	if len(r.workers) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), r.workersShutdownTimeout)
		var waitErr error
		for _, worker := range r.workers {
			if worker != nil {
				waitErr = errors.Join(waitErr, worker.Wait(ctx))
			}
		}
		cancel()
		if waitErr != nil {
			// Do not close dependencies under workers whose exits could not be
			// confirmed. The caller will terminate the process after this error.
			return waitErr
		}
	}
	// Flush whatever access_log rows are still queued before the database
	// connection closes. By this point every worker has been stopped and
	// joined above, and run()'s own call to runServersWithShutdownHook (and
	// therefore every server's http.Server.Shutdown) has already returned,
	// since this method runs from a defer registered before that call.
	r.accessWriter.Close()

	var closeErr error
	if r.store != nil {
		closeErr = errors.Join(closeErr, r.store.Close())
	}
	if r.database != nil {
		closeErr = errors.Join(closeErr, r.database.Close())
	}
	return closeErr
}

func newApplicationServer(address string, application http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           application,
		ReadHeaderTimeout: applicationReadHeaderTimeout,
		ReadTimeout:       applicationReadTimeout,
		WriteTimeout:      applicationWriteTimeout,
		IdleTimeout:       applicationIdleTimeout,
		MaxHeaderBytes:    applicationMaxHeaderBytes,
	}
}

func newRedirectServer(address string, redirect http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           redirect,
		ReadHeaderTimeout: redirectReadHeaderTimeout,
		ReadTimeout:       redirectReadTimeout,
		WriteTimeout:      redirectWriteTimeout,
		IdleTimeout:       redirectIdleTimeout,
		MaxHeaderBytes:    redirectMaxHeaderBytes,
	}
}

// newHTTPSRedirectHandler answers plain HTTP with a 308 to the HTTPS origin.
// The target host is chosen by hosts.RedirectHost, so an owner host stays on
// its own subdomain while the base host and anything unrecognised land on the
// base host. The Host header is only ever classified, never echoed, and
// forwarded headers are not consulted. The port comes from publicBaseURL alone;
// it is applied to owner hosts too because they sit behind the same listener.
func newHTTPSRedirectHandler(publicBaseURL string, hosts handler.HostModel) (http.Handler, error) {
	if strings.ContainsRune(publicBaseURL, '#') {
		return nil, errors.New("public base URL must not contain a fragment")
	}
	base, err := url.Parse(publicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse public base URL: %w", err)
	}
	if !strings.EqualFold(base.Scheme, "https") || base.Host == "" || base.Hostname() == "" || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" ||
		(base.Path != "" && base.Path != "/") {
		return nil, errors.New("public base URL must be a root-path HTTPS origin")
	}

	portSuffix := ""
	if port := base.Port(); port != "" {
		portSuffix = ":" + port
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := url.URL{Scheme: "https", Host: hosts.RedirectHost(r.Host) + portSuffix}
		target.Path = r.URL.Path
		target.RawPath = r.URL.RawPath
		if target.Path == "" {
			target.Path = "/"
		}
		target.RawQuery = r.URL.RawQuery
		w.Header().Set("Location", target.String())
		w.WriteHeader(http.StatusPermanentRedirect)
	}), nil
}

type managedServer struct {
	name   string
	server *http.Server
	serve  func() error
}

func manageHTTPServer(name string, server *http.Server) managedServer {
	return managedServer{name: name, server: server, serve: server.ListenAndServe}
}

type serverResult struct {
	name string
	err  error
}

// runServers treats the application and redirect listeners as one lifecycle:
// a signal or either listener exiting shuts down both listeners.
func runServers(ctx context.Context, shutdownTimeout time.Duration, servers ...managedServer) error {
	return runServersWithShutdownHook(ctx, shutdownTimeout, nil, servers...)
}

func runServersWithShutdownHook(ctx context.Context, shutdownTimeout time.Duration, shutdownStarted func(), servers ...managedServer) error {
	if len(servers) == 0 {
		return errors.New("no servers configured")
	}

	results := make(chan serverResult, len(servers))
	for _, managed := range servers {
		managed := managed
		go func() {
			results <- serverResult{name: managed.name, err: managed.serve()}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case result := <-results:
		if result.err == nil {
			runErr = fmt.Errorf("%s listener exited unexpectedly", result.name)
		} else {
			// runServers has not initiated shutdown yet, so even
			// http.ErrServerClosed here is an unexpected listener exit.
			runErr = fmt.Errorf("%s listener: %w", result.name, result.err)
		}
	}
	if shutdownStarted != nil {
		shutdownStarted()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErrs := make(chan error, len(servers))
	for _, managed := range servers {
		managed := managed
		go func() {
			if err := managed.server.Shutdown(shutdownCtx); err != nil {
				closeErr := managed.server.Close()
				shutdownErrs <- errors.Join(
					fmt.Errorf("shutdown %s listener: %w", managed.name, err),
					closeErr,
				)
				return
			}
			shutdownErrs <- nil
		}()
	}
	for range servers {
		runErr = errors.Join(runErr, <-shutdownErrs)
	}
	return runErr
}
