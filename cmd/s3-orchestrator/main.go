// -------------------------------------------------------------------------------
// S3 Orchestrator - Unified S3 Endpoint with Quota Management
//
// Author: Alex Freidah
//
// Entry point for the S3 proxy service. Dispatches to subcommands: "serve"
// (default) starts the HTTP server, "sync" imports pre-existing bucket objects
// into the proxy's metadata database, "version" prints build info, and
// "validate" checks a configuration file without starting the server.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

func main() { // codecov:ignore -- thin wrapper, logic tested via run()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sync":
			os.Args = os.Args[1:]
			runSync()
			return
		case "version":
			runVersion()
			return
		case "validate":
			os.Args = os.Args[1:]
			runValidate()
			return
		case "init":
			os.Args = os.Args[1:]
			runInit()
			return
		case "admin":
			os.Args = os.Args[1:]
			runAdmin()
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}
	runServe()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: s3-orchestrator [command]

Commands:
  (default)   Start the S3 proxy server
  init        Generate a configuration file interactively
  admin       Operational CLI for a running instance
  sync        Import pre-existing bucket objects into the database
  validate    Check a configuration file without starting the server
  version     Print version and build info
  help        Show this help message

Run 's3-orchestrator <command> --help' for command-specific flags.
`)
}

func runServe() { // codecov:ignore -- flag parsing + os.Exit wrapper
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	mode := flag.String("mode", "all", "Operating mode: api, worker, or all")
	flag.Parse()

	switch *mode {
	case "api", "worker", "all":
	default:
		fmt.Fprintf(os.Stderr, "invalid mode %q: must be api, worker, or all\n", *mode)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *configPath, *mode, os.Stdout); err != nil {
		slog.ErrorContext(ctx, "Server error", "error", err)
		os.Exit(1)
	}
}

// -------------------------------------------------------------------------
// SERVER
// -------------------------------------------------------------------------

// server holds resolved services and runtime state shared across the serve
// lifecycle (startup, SIGHUP reload, shutdown). Grouping these into a struct
// avoids passing dozens of locals between lifecycle methods and makes each
// phase independently testable.
type server struct {
	configPath string
	mode       string
	stdout     io.Writer

	cfg    *config.Config
	cfgPtr syncutil.AtomicConfig[config.Config]

	ready    atomic.Bool
	logLevel slog.LevelVar

	logBuffer      *telemetry.LogBuffer
	shutdownTracer func(ctx context.Context) error

	inj     do.Injector
	db      store.AdminStore
	cbStore *store.CircuitBreakerStore
	manager *proxy.BackendManager
	srv     *s3api.Server
	sm      *lifecycle.Manager

	rl            *s3api.RateLimiter
	uiHandler     *ui.Handler
	loginThrottle *httputil.LoginThrottle
	certReloader  *httputil.CertReloader

	httpServer    *http.Server
	metricsServer *http.Server

	bgCancel context.CancelFunc
	bgDone   chan struct{}

	// hupChan receives SIGHUP signals for config reload. In production this
	// is wired to signal.Notify; tests can send on it directly.
	hupChan chan os.Signal
	hupDone chan struct{}
}

// run is the core server lifecycle: load config, wire dependencies, start
// the HTTP server, and block until ctx is cancelled (SIGINT/SIGTERM). All
// errors are returned rather than calling os.Exit, making the function
// testable from unit tests.
func run(ctx context.Context, configPath, mode string, stdout io.Writer) error {
	s := &server{
		configPath: configPath,
		mode:       mode,
		stdout:     stdout,
	}

	if err := s.loadConfig(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := s.initLogging(); err != nil {
		return fmt.Errorf("init logging: %w", err)
	}
	s.initDI()
	if err := s.resolveServices(); err != nil {
		return fmt.Errorf("resolve services: %w", err)
	}
	s.buildHTTPServer()
	if err := s.configureTLS(); err != nil {
		return fmt.Errorf("configure TLS: %w", err)
	}
	s.startBackgroundServices()
	s.startSIGHUPHandler()

	s.ready.Store(true)
	s.logStartup()

	// Block on the HTTP listener. Shutdown is triggered when ctx is
	// cancelled (SIGINT/SIGTERM), which calls httpServer.Shutdown from
	// the shutdown goroutine.
	serverErr := make(chan error, 1)
	go func() {
		if s.httpServer.TLSConfig != nil {
			serverErr <- s.httpServer.ListenAndServeTLS("", "")
		} else {
			serverErr <- s.httpServer.ListenAndServe()
		}
	}()

	// Wait for either the context to be cancelled or the server to exit.
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}

	s.shutdown()

	// Drain any server exit error after shutdown.
	if err := <-serverErr; err != nil && err != http.ErrServerClosed {
		return err
	}

	slog.InfoContext(ctx, "Server stopped")
	return nil
}

// -------------------------------------------------------------------------
// INITIALIZATION
// -------------------------------------------------------------------------

// loadConfig reads and validates the YAML configuration file.
func (s *server) loadConfig() error {
	cfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// initLogging configures structured JSON logging with the configured level,
// initializes OpenTelemetry tracing, and sets the build info metric.
func (s *server) initLogging() error {
	s.logLevel.Set(config.ParseLogLevel(s.cfg.Server.LogLevel))
	s.logBuffer = telemetry.NewLogBuffer()
	jsonHandler := slog.NewJSONHandler(s.stdout, &slog.HandlerOptions{Level: &s.logLevel})
	traceHandler := telemetry.NewTraceHandler(jsonHandler)
	slog.SetDefault(slog.New(telemetry.NewTeeHandler(traceHandler, s.logBuffer)))

	ctx := context.Background()
	shutdownTracer, err := telemetry.InitTracer(ctx, s.cfg.Telemetry.Tracing)
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}
	s.shutdownTracer = shutdownTracer

	telemetry.BuildInfo.WithLabelValues(telemetry.Version, runtime.Version()).Set(1)
	wireAuditMetrics()
	return nil
}

// initDI creates the dependency injection container and registers all
// providers. Optional providers are registered only when the corresponding
// config section is enabled.
func (s *server) initDI() {
	cfg := s.cfg

	s.inj = do.New()
	do.ProvideValue(s.inj, cfg)
	do.ProvideValue(s.inj, s.mode)
	do.ProvideValue(s.inj, &s.logLevel)
	do.ProvideValue(s.inj, s.logBuffer)

	do.Provide(s.inj, ProvideStoreBundle)
	do.Provide(s.inj, ProvideMetadataStore)
	do.Provide(s.inj, ProvideAdminStore)
	do.Provide(s.inj, ProvideCBStore)
	do.Provide(s.inj, ProvideBackends)
	do.Provide(s.inj, ProvideBackendManager)
	do.Provide(s.inj, ProvideBucketAuth)
	do.Provide(s.inj, ProvideS3Server)
	do.Provide(s.inj, ProvideLifecycleManager)

	if cfg.Encryption.Enabled {
		do.Provide(s.inj, ProvideEncryptor)
		do.Provide(s.inj, ProvideEncryptionProvider)
	}
	if cfg.Redis != nil {
		do.Provide(s.inj, ProvideRedisCounterBackend)
	}
	if cfg.Cache.Enabled {
		do.Provide(s.inj, ProvideObjectCache)
	}
	if cfg.RateLimit.Enabled {
		do.Provide(s.inj, ProvideRateLimiter)
	}
	if cfg.UI.Enabled {
		do.Provide(s.inj, ProvideLoginThrottle)
		do.Provide(s.inj, ProvideUIHandler)
	}
	if cfg.UI.AdminKey != "" {
		do.Provide(s.inj, ProvideAdminHandler)
	}
	if len(cfg.Notifications.Endpoints) > 0 {
		do.Provide(s.inj, ProvideNotifier)
	}
}

// resolveServices triggers lazy construction of core services from the DI
// container, stores initial reloadable configs on the manager, and seeds
// the quota metrics gauge.
func (s *server) resolveServices() error {
	s.db = do.MustInvoke[store.AdminStore](s.inj)
	s.cbStore = do.MustInvoke[*store.CircuitBreakerStore](s.inj)
	s.manager = do.MustInvoke[*proxy.BackendManager](s.inj)
	s.srv = do.MustInvoke[*s3api.Server](s.inj)

	// Reloadable configs must be set before the lifecycle manager is
	// constructed because service constructors read worker intervals at
	// creation time.
	s.manager.Rebalancer.SetConfig(&s.cfg.Rebalance)
	s.manager.Replicator.SetConfig(&s.cfg.Replication)
	s.manager.OverReplicationCleaner.SetConfig(&s.cfg.Replication)
	s.manager.SetUsageFlushConfig(&s.cfg.UsageFlush)
	s.manager.SetLifecycleConfig(&s.cfg.Lifecycle)
	s.manager.SetIntegrityConfig(&s.cfg.Integrity)

	s.sm = do.MustInvoke[*lifecycle.Manager](s.inj)

	s.cfgPtr.Store(s.cfg)

	ctx := context.Background()
	if err := s.manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to update initial quota metrics", "error", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// HTTP SERVER
// -------------------------------------------------------------------------

// buildHTTPServer assembles the HTTP mux with health endpoints, metrics,
// admin API, UI dashboard, and S3 proxy handler, then creates the
// http.Server with configured timeouts.
func (s *server) buildHTTPServer() {
	cfg := s.cfg
	ctx := context.Background()
	mux := http.NewServeMux()

	s.configureMetrics(mux, ctx)
	s.registerHealthEndpoints(mux)

	if r, err := do.Invoke[*s3api.RateLimiter](s.inj); err == nil {
		s.rl = r
	}

	if adminHandler, err := do.Invoke[*admin.Handler](s.inj); err == nil {
		adminMux := http.NewServeMux()
		adminHandler.Register(adminMux)
		var adminHTTP http.Handler = adminMux
		if s.rl != nil {
			adminHTTP = s.rl.Middleware(adminHTTP)
		}
		mux.Handle("/admin/", adminHTTP)
		slog.InfoContext(ctx, "Admin API enabled", "path", "/admin/api/")
	}

	if s.mode == "api" || s.mode == "all" {
		s.registerUIHandler(mux, ctx)
		s.registerS3Handler(mux)
	}

	s.httpServer = &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}
}

// configureMetrics sets up the Prometheus metrics endpoint, either on a
// separate listener or on the main mux.
func (s *server) configureMetrics(mux *http.ServeMux, ctx context.Context) {
	cfg := s.cfg
	if !cfg.Telemetry.Metrics.Enabled {
		return
	}

	if cfg.Telemetry.Metrics.Listen != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		s.metricsServer = &http.Server{
			Addr:              cfg.Telemetry.Metrics.Listen,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.InfoContext(ctx, "Metrics endpoint enabled on separate listener",
				"listen", cfg.Telemetry.Metrics.Listen, "path", cfg.Telemetry.Metrics.Path)
			if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.ErrorContext(ctx, "Metrics listener failed", "error", err)
			}
		}()
	} else {
		mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		slog.InfoContext(ctx, "Metrics endpoint enabled", "path", cfg.Telemetry.Metrics.Path)
	}
}

// registerHealthEndpoints adds /health and /health/ready to the mux.
func (s *server) registerHealthEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if !s.cbStore.IsHealthy() {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not ready"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
}

// registerUIHandler wires the optional web UI dashboard and login throttle.
func (s *server) registerUIHandler(mux *http.ServeMux, ctx context.Context) {
	if h, err := do.Invoke[*ui.Handler](s.inj); err == nil {
		s.uiHandler = h
		s.uiHandler.Register(mux, s.cfg.UI.Path)
		slog.InfoContext(ctx, "Web UI enabled", "path", s.cfg.UI.Path)
	}
	if lt, err := do.Invoke[*httputil.LoginThrottle](s.inj); err == nil {
		s.loginThrottle = lt
	}
}

// registerS3Handler builds the S3 proxy handler with optional rate limiting
// and admission control, then mounts it on the root path.
func (s *server) registerS3Handler(mux *http.ServeMux) {
	cfg := s.cfg
	var s3Handler http.Handler = s.srv

	if s.rl != nil {
		s3Handler = s.rl.Middleware(s3Handler)
	}

	if cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0 {
		readSem := make(chan struct{}, cfg.Server.MaxConcurrentReads)
		ac := s3api.NewSplitAdmissionControllerFromSem(readSem, s.manager.AdmissionSem())
		if cfg.Server.LoadShedThreshold > 0 {
			ac.SetShedThreshold(cfg.Server.LoadShedThreshold)
		}
		if cfg.Server.AdmissionWait > 0 {
			ac.SetAdmissionWait(cfg.Server.AdmissionWait)
		}
		s3Handler = ac.Middleware(s3Handler)
	} else if cfg.Server.MaxConcurrentRequests > 0 {
		ac := s3api.NewAdmissionControllerFromSem(s.manager.AdmissionSem())
		if cfg.Server.LoadShedThreshold > 0 {
			ac.SetShedThreshold(cfg.Server.LoadShedThreshold)
		}
		if cfg.Server.AdmissionWait > 0 {
			ac.SetAdmissionWait(cfg.Server.AdmissionWait)
		}
		s3Handler = ac.Middleware(s3Handler)
	}

	mux.Handle("/", s3Handler)
}

// -------------------------------------------------------------------------
// TLS
// -------------------------------------------------------------------------

// configureTLS loads the server certificate and optional client CA for mTLS.
// Returns nil when TLS is not configured.
func (s *server) configureTLS() error {
	cfg := s.cfg
	if cfg.Server.TLS.CertFile == "" {
		return nil
	}

	var err error
	s.certReloader, err = httputil.NewCertReloader(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}

	tlsCfg := &tls.Config{ //nolint:gosec // G402: MinVersion set from config, defaults to TLS 1.2
		GetCertificate: s.certReloader.GetCertificate,
		MinVersion:     parseTLSVersion(cfg.Server.TLS.MinVersion),
	}

	if cfg.Server.TLS.ClientCAFile != "" {
		caCert, err := os.ReadFile(cfg.Server.TLS.ClientCAFile)
		if err != nil {
			return fmt.Errorf("read client CA file: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return fmt.Errorf("parse client CA certificate: no valid PEM blocks found")
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
	}

	s.httpServer.TLSConfig = tlsCfg
	return nil
}

// -------------------------------------------------------------------------
// BACKGROUND SERVICES
// -------------------------------------------------------------------------

// startBackgroundServices launches the lifecycle manager in a background
// goroutine. Cancel bgCancel to stop all managed services.
func (s *server) startBackgroundServices() {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s.bgCancel = bgCancel
	s.bgDone = make(chan struct{})
	go func() {
		s.sm.Run(bgCtx)
		close(s.bgDone)
	}()
}

// -------------------------------------------------------------------------
// CONFIG RELOAD (SIGHUP)
// -------------------------------------------------------------------------

// startSIGHUPHandler spawns a goroutine that reloads configuration on
// SIGHUP. The hupChan field is exposed so tests can trigger reloads
// without real signals.
func (s *server) startSIGHUPHandler() {
	s.hupChan = make(chan os.Signal, 1)
	s.hupDone = make(chan struct{})
	signal.Notify(s.hupChan, syscall.SIGHUP)

	go func() {
		defer close(s.hupDone)
		for range s.hupChan {
			s.reloadConfig()
		}
	}()
}

// reloadConfig handles a single SIGHUP-triggered configuration reload.
// Reloads bucket credentials, rate limits, quota limits, worker configs,
// log level, and TLS certificates.
func (s *server) reloadConfig() {
	ctx := context.Background()
	slog.InfoContext(ctx, "SIGHUP received, reloading configuration", "path", s.configPath)

	newCfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		slog.ErrorContext(ctx, "Config reload failed, keeping current config", "error", err)
		return
	}

	currentCfg := s.cfgPtr.Load()
	if warnings := config.NonReloadableFieldsChanged(currentCfg, newCfg); len(warnings) > 0 {
		for _, w := range warnings {
			slog.WarnContext(ctx, "Config field changed but requires restart to take effect", "field", w)
		}
	}

	if s.certReloader != nil {
		if err := s.certReloader.Reload(); err != nil {
			slog.ErrorContext(ctx, "Failed to reload TLS certificate", "error", err)
		}
	}

	s.srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
	slog.InfoContext(ctx, "Reloaded bucket credentials", "buckets", len(newCfg.Buckets))

	if s.rl != nil && newCfg.RateLimit.Enabled {
		s.rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
	}

	reloadCtx, reloadCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := s.db.SyncQuotaLimits(reloadCtx, newCfg.Backends); err != nil {
		slog.ErrorContext(reloadCtx, "Failed to sync quota limits on reload", "error", err)
	}

	newUsageLimits := make(map[string]store.UsageLimits, len(newCfg.Backends))
	for i := range newCfg.Backends {
		bcfg := &newCfg.Backends[i]
		newUsageLimits[bcfg.Name] = store.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
	}
	s.manager.UpdateUsageLimits(newUsageLimits)

	s.logLevel.Set(config.ParseLogLevel(newCfg.Server.LogLevel))

	s.manager.Rebalancer.SetConfig(&newCfg.Rebalance)
	s.manager.Replicator.SetConfig(&newCfg.Replication)
	s.manager.OverReplicationCleaner.SetConfig(&newCfg.Replication)
	s.manager.SetUsageFlushConfig(&newCfg.UsageFlush)
	s.manager.SetLifecycleConfig(&newCfg.Lifecycle)
	s.manager.SetIntegrityConfig(&newCfg.Integrity)

	if err := s.manager.UpdateQuotaMetrics(reloadCtx); err != nil {
		slog.WarnContext(reloadCtx, "Failed to update quota metrics after reload", "error", err)
	}
	reloadCancel()

	if s.uiHandler != nil {
		s.uiHandler.UpdateConfig(newCfg)
	}

	s.cfgPtr.Store(newCfg)
	slog.InfoContext(ctx, "Configuration reload complete")
}

// -------------------------------------------------------------------------
// SHUTDOWN
// -------------------------------------------------------------------------

// shutdown performs ordered teardown of all server components. Called after
// the HTTP server has stopped accepting new connections.
func (s *server) shutdown() {
	ctx := context.Background()
	slog.InfoContext(ctx, "Shutting down")

	if delay := s.cfgPtr.Load().Server.ShutdownDelay; delay > 0 {
		slog.InfoContext(ctx, "Waiting for load balancer deregistration", "delay", delay)
		time.Sleep(delay)
	}

	s.ready.Store(false)

	// Stop SIGHUP handler before tearing down services it touches.
	signal.Stop(s.hupChan)
	close(s.hupChan)
	<-s.hupDone

	// Single 30-second root deadline bounds the entire shutdown sequence.
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "HTTP server shutdown error", "error", err)
	}

	if s.metricsServer != nil {
		if err := s.metricsServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Metrics server shutdown error", "error", err)
		}
	}

	if s.rl != nil {
		s.rl.Close()
	}
	if s.loginThrottle != nil {
		s.loginThrottle.Close()
	}

	s.bgCancel()
	<-s.bgDone
	s.sm.Stop(10 * time.Second)

	if encProvider, err := do.Invoke[encryption.KeyProvider](s.inj); err == nil {
		if closer, ok := encProvider.(interface{ Close() }); ok {
			closer.Close()
		}
	}

	s.manager.Close()

	if err := s.manager.FlushUsage(shutdownCtx); err != nil {
		slog.WarnContext(shutdownCtx, "Final usage flush failed", "error", err)
	}

	if redisBackend, err := do.Invoke[*counter.RedisCounterBackend](s.inj); err == nil {
		if err := redisBackend.Close(); err != nil {
			slog.WarnContext(shutdownCtx, "Failed to close Redis client", "error", err)
		}
	}

	s.db.Close()

	if err := s.shutdownTracer(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "Tracer shutdown error", "error", err)
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// logStartup emits the server startup log entry with key configuration.
func (s *server) logStartup() {
	bucketNames := make([]string, len(s.cfg.Buckets))
	for i, b := range s.cfg.Buckets {
		bucketNames[i] = b.Name
	}
	slog.InfoContext(context.Background(), "S3 Orchestrator starting",
		"version", telemetry.Version,
		"mode", s.mode,
		"listen_addr", s.cfg.Server.ListenAddr,
		"log_level", s.cfg.Server.LogLevel,
		"buckets", bucketNames,
		"backends", len(s.cfg.Backends),
		"routing_strategy", s.cfg.RoutingStrategy,
	)
}

// parseTLSVersion maps a config string to a tls.VersionTLS constant.
func parseTLSVersion(v string) uint16 {
	switch v {
	case "1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}
