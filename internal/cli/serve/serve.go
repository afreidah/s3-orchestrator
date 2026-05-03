// -------------------------------------------------------------------------------
// Serve - Server Lifecycle for the s3-orchestrator Daemon
//
// Author: Alex Freidah
//
// Owns the long-running daemon lifecycle: load config, wire dependencies,
// start the HTTP server(s), reload on SIGHUP, and shut down cleanly. The cmd/
// package only forwards os.Args + signal context here. All DI-resolved
// services live in the injector; the server struct holds only state that
// genuinely persists across phases (atomic config, lifecycle handles).
// -------------------------------------------------------------------------------

package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// server holds the runtime state shared across the serve lifecycle (startup,
// SIGHUP reload, shutdown). DI-resolved services are not cached here; they
// are fetched from the injector on demand so a single resolution path
// answers every consumer (handlers, reload, shutdown).
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

	inj          do.Injector
	certReloader *httputil.CertReloader

	httpServer    *http.Server
	metricsServer *http.Server

	bgCancel context.CancelFunc
	bgDone   chan struct{}

	// hupChan receives SIGHUP signals for config reload. In production this
	// is wired to signal.Notify; tests can send on it directly.
	hupChan chan os.Signal
	hupDone chan struct{}
}

// Run is the daemon entry point. Loads config, wires dependencies, starts the
// HTTP server, and blocks until ctx is cancelled (SIGINT/SIGTERM). All errors
// are returned rather than calling os.Exit, making the function testable from
// unit tests.
func Run(ctx context.Context, configPath, mode string, stdout io.Writer) error {
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
	if err := s.buildHTTPServer(); err != nil {
		return fmt.Errorf("build HTTP server: %w", err)
	}
	if err := s.configureTLS(); err != nil {
		return fmt.Errorf("configure TLS: %w", err)
	}
	s.startBackgroundServices()
	s.startSIGHUPHandler()

	s.ready.Store(true)
	s.logStartup()

	serverErr := make(chan error, 1)
	go func() {
		if s.httpServer.TLSConfig != nil {
			serverErr <- s.httpServer.ListenAndServeTLS("", "")
		} else {
			serverErr <- s.httpServer.ListenAndServe()
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}

	s.shutdown()

	if err := <-serverErr; err != nil && err != http.ErrServerClosed {
		return err
	}

	slog.InfoContext(ctx, "server stopped")
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
	di.WireAuditMetrics()
	return nil
}

// initDI creates the dependency injection container via di.NewInjector.
func (s *server) initDI() {
	s.inj = di.NewInjector(s.cfg, s.mode, &s.logLevel, s.logBuffer)
}

// resolveServices triggers lazy construction of all required services and
// applies the runtime configurations the lifecycle manager reads at startup.
// Optional services are constructed lazily by their first invoker; this
// function only catches errors that should be fatal at boot.
func (s *server) resolveServices() error {
	if _, err := do.Invoke[core.LifecycleAdmin](s.inj); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	if _, err := do.Invoke[*breaker.CircuitBreaker](s.inj); err != nil {
		return fmt.Errorf("initialize database circuit breaker: %w", err)
	}
	manager, err := do.Invoke[*proxy.BackendManager](s.inj)
	if err != nil {
		return fmt.Errorf("initialize backend manager: %w", err)
	}
	if _, err := do.Invoke[*s3api.Server](s.inj); err != nil {
		return fmt.Errorf("initialize S3 server: %w", err)
	}

	if err := s.applyWorkerConfigs(&s.cfg.Rebalance, &s.cfg.Replication, &s.cfg.Integrity); err != nil {
		return err
	}
	manager.SetUsageFlushConfig(&s.cfg.UsageFlush)
	manager.SetLifecycleConfig(&s.cfg.Lifecycle)
	manager.SetIntegrityConfig(&s.cfg.Integrity)

	if _, err := do.Invoke[*lifecycle.Manager](s.inj); err != nil {
		return fmt.Errorf("initialize lifecycle manager: %w", err)
	}

	if s.cfg.RateLimit.Enabled {
		if _, err := do.Invoke[*s3api.RateLimiter](s.inj); err != nil {
			return fmt.Errorf("initialize rate limiter: %w", err)
		}
	}
	if s.cfg.UI.Enabled {
		if _, err := do.Invoke[*httputil.LoginThrottle](s.inj); err != nil {
			return fmt.Errorf("initialize login throttle: %w", err)
		}
	}

	s.cfgPtr.Store(s.cfg)

	ctx := context.Background()
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "failed to update initial quota metrics", "error", err)
	}
	return nil
}

// invokeOptional wraps do.Invoke to return nil instead of an error when the
// provider isn't registered (the standard DI signal for "feature disabled").
func invokeOptional[T any](inj do.Injector) T {
	v, _ := do.Invoke[T](inj)
	return v
}

// adminStore returns the resolved LifecycleAdmin. Fatal errors at boot
// have already surfaced via resolveServices, so nil is impossible after Run
// progresses past that step.
func (s *server) adminStore() core.LifecycleAdmin {
	v, _ := do.Invoke[core.LifecycleAdmin](s.inj)
	return v
}

// dbCB returns the resolved database circuit breaker.
func (s *server) dbCB() *breaker.CircuitBreaker {
	v, _ := do.Invoke[*breaker.CircuitBreaker](s.inj)
	return v
}

// backendManager returns the resolved BackendManager.
func (s *server) backendManager() *proxy.BackendManager {
	v, _ := do.Invoke[*proxy.BackendManager](s.inj)
	return v
}

// s3Server returns the resolved S3 server handler.
func (s *server) s3Server() *s3api.Server {
	v, _ := do.Invoke[*s3api.Server](s.inj)
	return v
}

// lifecycleManager returns the resolved lifecycle manager.
func (s *server) lifecycleManager() *lifecycle.Manager {
	v, _ := do.Invoke[*lifecycle.Manager](s.inj)
	return v
}

// applyWorkerConfigs pushes the (possibly hot-reloaded) Rebalance,
// Replication, and Integrity configs onto each worker. Workers are
// resolved from DI on every call so a not-yet-constructed worker (e.g.
// in api mode) doesn't crash boot.
func (s *server) applyWorkerConfigs(rebalance *config.RebalanceConfig, replication *config.ReplicationConfig, integrity *config.IntegrityConfig) error {
	if rb, err := do.Invoke[*worker.Rebalancer](s.inj); err == nil {
		rb.SetConfig(rebalance)
	}
	if rp, err := do.Invoke[*worker.Replicator](s.inj); err == nil {
		rp.SetConfig(replication)
	}
	if or, err := do.Invoke[*worker.OverReplicationCleaner](s.inj); err == nil {
		or.SetConfig(replication)
	}
	if sc, err := do.Invoke[*worker.Scrubber](s.inj); err == nil {
		sc.SetConfig(integrity)
	}
	return nil
}

// -------------------------------------------------------------------------
// HTTP SERVER
// -------------------------------------------------------------------------

// buildHTTPServer assembles the HTTP mux with health endpoints, metrics,
// admin API, UI dashboard, and S3 proxy handler, then creates the
// http.Server with configured timeouts.
func (s *server) buildHTTPServer() error {
	cfg := s.cfg
	ctx := context.Background()
	mux := http.NewServeMux()

	s.configureMetrics(mux, ctx)
	s.registerHealthEndpoints(mux)

	if cfg.UI.AdminKey != "" {
		adminHandler, err := do.Invoke[*admin.Handler](s.inj)
		if err != nil {
			return fmt.Errorf("initialize admin handler: %w", err)
		}
		adminMux := http.NewServeMux()
		adminHandler.Register(adminMux)
		var adminHTTP http.Handler = adminMux
		if rl := invokeOptional[*s3api.RateLimiter](s.inj); rl != nil {
			adminHTTP = rl.Middleware(adminHTTP)
		}
		mux.Handle("/admin/", adminHTTP)
		slog.InfoContext(ctx, "admin API enabled", "path", "/admin/api/")
	}

	if s.mode == "api" || s.mode == "all" {
		if err := s.registerUIHandler(mux, ctx); err != nil {
			return err
		}
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
	return nil
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
			slog.InfoContext(ctx, "metrics endpoint enabled on separate listener",
				"listen", cfg.Telemetry.Metrics.Listen, "path", cfg.Telemetry.Metrics.Path)
			if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.ErrorContext(ctx, "metrics listener failed", "error", err)
			}
		}()
	} else {
		mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		slog.InfoContext(ctx, "metrics endpoint enabled", "path", cfg.Telemetry.Metrics.Path)
	}
}

// registerHealthEndpoints adds /health and /health/ready to the mux.
func (s *server) registerHealthEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if cb := s.dbCB(); cb != nil && !cb.IsHealthy() {
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

// registerUIHandler wires the optional web UI dashboard.
func (s *server) registerUIHandler(mux *http.ServeMux, ctx context.Context) error {
	if !s.cfg.UI.Enabled {
		return nil
	}
	h, err := do.Invoke[*ui.Handler](s.inj)
	if err != nil {
		return fmt.Errorf("initialize UI handler: %w", err)
	}
	h.Register(mux, s.cfg.UI.Path)
	slog.InfoContext(ctx, "web UI enabled", "path", s.cfg.UI.Path)
	return nil
}

// registerS3Handler builds the S3 proxy handler with optional rate limiting
// and admission control, then mounts it on the root path.
func (s *server) registerS3Handler(mux *http.ServeMux) {
	cfg := s.cfg
	manager := s.backendManager()
	var s3Handler http.Handler = s.s3Server()

	if rl := invokeOptional[*s3api.RateLimiter](s.inj); rl != nil {
		s3Handler = rl.Middleware(s3Handler)
	}

	if cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0 {
		readSem := make(chan struct{}, cfg.Server.MaxConcurrentReads)
		ac := s3api.NewSplitAdmissionControllerFromSem(readSem, manager.AdmissionSem())
		if cfg.Server.LoadShedThreshold > 0 {
			ac.SetShedThreshold(cfg.Server.LoadShedThreshold)
		}
		if cfg.Server.AdmissionWait > 0 {
			ac.SetAdmissionWait(cfg.Server.AdmissionWait)
		}
		s3Handler = ac.Middleware(s3Handler)
	} else if cfg.Server.MaxConcurrentRequests > 0 {
		ac := s3api.NewAdmissionControllerFromSem(manager.AdmissionSem())
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
	sm := s.lifecycleManager()
	go func() {
		sm.Run(bgCtx)
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

// reloadConfig handles a single SIGHUP-triggered reload. The new config is
// loaded and validated up front; any failure aborts the reload before any
// atomic state has been touched. Once validation passes the changes are
// applied in a single block so partial-failure cannot leave the process
// half-reloaded.
func (s *server) reloadConfig() {
	ctx := context.Background()
	slog.InfoContext(ctx, "SIGHUP received, reloading configuration", "path", s.configPath)

	newCfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		slog.ErrorContext(ctx, "config reload aborted, keeping current config", "error", err)
		return
	}

	currentCfg := s.cfgPtr.Load()
	if warnings := config.NonReloadableFieldsChanged(currentCfg, newCfg); len(warnings) > 0 {
		for _, w := range warnings {
			slog.WarnContext(ctx, "config field changed but requires restart to take effect", "field", w)
		}
	}

	s.applyReload(ctx, newCfg)
	s.cfgPtr.Store(newCfg)
	slog.InfoContext(ctx, "configuration reload complete")
}

// applyReload performs the swap-and-apply phase. Each section is applied
// independently and logs its own outcome; failures in non-fatal sections
// (quota sync, metrics update) are logged but do not abort the reload.
func (s *server) applyReload(ctx context.Context, newCfg *config.Config) {
	if s.certReloader != nil {
		if err := s.certReloader.Reload(); err != nil {
			slog.ErrorContext(ctx, "failed to reload TLS certificate", "error", err)
		}
	}

	s.s3Server().SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
	slog.InfoContext(ctx, "reloaded bucket credentials", "buckets", len(newCfg.Buckets))

	if rl := invokeOptional[*s3api.RateLimiter](s.inj); rl != nil && newCfg.RateLimit.Enabled {
		rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
	}

	reloadCtx, reloadCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reloadCancel()

	if err := s.adminStore().SyncQuotaLimits(reloadCtx, newCfg.Backends); err != nil {
		slog.ErrorContext(reloadCtx, "failed to sync quota limits on reload", "error", err)
	}

	manager := s.backendManager()
	newUsageLimits := make(map[string]core.UsageLimits, len(newCfg.Backends))
	for i := range newCfg.Backends {
		bcfg := &newCfg.Backends[i]
		newUsageLimits[bcfg.Name] = core.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
	}
	manager.UpdateUsageLimits(newUsageLimits)

	s.logLevel.Set(config.ParseLogLevel(newCfg.Server.LogLevel))

	if err := s.applyWorkerConfigs(&newCfg.Rebalance, &newCfg.Replication, &newCfg.Integrity); err != nil {
		slog.WarnContext(reloadCtx, "failed to apply worker configs", "error", err)
	}
	manager.SetUsageFlushConfig(&newCfg.UsageFlush)
	manager.SetLifecycleConfig(&newCfg.Lifecycle)
	manager.SetIntegrityConfig(&newCfg.Integrity)

	if err := manager.UpdateQuotaMetrics(reloadCtx); err != nil {
		slog.WarnContext(reloadCtx, "failed to update quota metrics after reload", "error", err)
	}

	if h := invokeOptional[*ui.Handler](s.inj); h != nil {
		h.UpdateConfig(newCfg)
	}
}

// -------------------------------------------------------------------------
// SHUTDOWN
// -------------------------------------------------------------------------

// shutdown performs ordered teardown of all server components. Called after
// the HTTP server has stopped accepting new connections.
func (s *server) shutdown() {
	ctx := context.Background()
	slog.InfoContext(ctx, "shutting down")

	// Deferred so DI-managed resources (providers implementing do.Shutdownable)
	// are always released, even if an earlier shutdown step panics or aborts.
	defer func() {
		if s.inj != nil {
			_ = s.inj.Shutdown()
		}
	}()

	if delay := s.cfgPtr.Load().Server.ShutdownDelay; delay > 0 {
		slog.InfoContext(ctx, "waiting for load balancer deregistration", "delay", delay)
		time.Sleep(delay)
	}

	s.ready.Store(false)

	signal.Stop(s.hupChan)
	close(s.hupChan)
	<-s.hupDone

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	s.shutdownHTTPServers(shutdownCtx)

	if rl := invokeOptional[*s3api.RateLimiter](s.inj); rl != nil {
		rl.Close()
	}
	if lt := invokeOptional[*httputil.LoginThrottle](s.inj); lt != nil {
		lt.Close()
	}

	s.bgCancel()
	<-s.bgDone
	s.lifecycleManager().Stop(10 * time.Second)

	s.closeEncryptionProvider()
	manager := s.backendManager()
	manager.Close()

	if err := manager.FlushUsage(shutdownCtx); err != nil {
		slog.WarnContext(shutdownCtx, "final usage flush failed", "error", err)
	}

	s.closeRedisBackend(shutdownCtx)
	s.adminStore().Close()

	if err := s.shutdownTracer(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "tracer shutdown error", "error", err)
	}
}

// shutdownHTTPServers tears down the main HTTP server and the optional
// metrics listener, logging any errors without aborting shutdown.
func (s *server) shutdownHTTPServers(ctx context.Context) {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		slog.ErrorContext(ctx, "HTTP server shutdown error", "error", err)
	}
	if s.metricsServer != nil {
		if err := s.metricsServer.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "metrics server shutdown error", "error", err)
		}
	}
}

// closeEncryptionProvider closes the encryption key provider if one was
// resolved from the injector and implements an optional Close method.
func (s *server) closeEncryptionProvider() {
	encProvider, err := do.Invoke[encryption.KeyProvider](s.inj)
	if err != nil {
		return
	}
	if closer, ok := encProvider.(interface{ Close() }); ok {
		closer.Close()
	}
}

// closeRedisBackend closes the Redis counter backend if one was registered.
func (s *server) closeRedisBackend(ctx context.Context) {
	redisBackend, err := do.Invoke[*counter.RedisCounterBackend](s.inj)
	if err != nil {
		return
	}
	if err := redisBackend.Close(); err != nil {
		slog.WarnContext(ctx, "failed to close Redis client", "error", err)
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
