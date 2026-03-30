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

	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
)

func main() {

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

func runServe() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	mode := flag.String("mode", "all", "Operating mode: api, worker, or all")
	flag.Parse()

	// --- Validate mode ---
	switch *mode {
	case "api", "worker", "all":
	default:
		fmt.Fprintf(os.Stderr, "invalid mode %q: must be api, worker, or all\n", *mode)
		os.Exit(1)
	}

	// --- Readiness gate ---
	var ready atomic.Bool

	// --- Load configuration ---
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// --- Initialize structured logger with configurable level ---
	var logLevel slog.LevelVar
	logLevel.Set(config.ParseLogLevel(cfg.Server.LogLevel))
	logBuffer := telemetry.NewLogBuffer()
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	traceHandler := telemetry.NewTraceHandler(jsonHandler)
	slog.SetDefault(slog.New(telemetry.NewTeeHandler(traceHandler, logBuffer)))

	// --- Initialize tracing ---
	ctx := context.Background()
	shutdownTracer, err := telemetry.InitTracer(ctx, cfg.Telemetry.Tracing)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to initialize tracer", "error", err)
		os.Exit(1)
	}

	// --- Set build info metric ---
	telemetry.BuildInfo.WithLabelValues(telemetry.Version, runtime.Version()).Set(1)

	// --- Wire audit event counter ---
	wireAuditMetrics()

	// --- Create DI injector and register core values ---
	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.ProvideValue(inj, *mode)
	do.ProvideValue(inj, &logLevel)
	do.ProvideValue(inj, logBuffer)

	// --- Register infrastructure providers ---
	do.Provide(inj, ProvideStoreBundle)
	do.Provide(inj, ProvideMetadataStore)
	do.Provide(inj, ProvideAdminStore)
	do.Provide(inj, ProvideCBStore)
	do.Provide(inj, ProvideBackends)
	do.Provide(inj, ProvideBackendManager)
	do.Provide(inj, ProvideBucketAuth)
	do.Provide(inj, ProvideS3Server)
	do.Provide(inj, ProvideLifecycleManager)

	// --- Register optional providers (only when config enables them) ---
	if cfg.Encryption.Enabled {
		do.Provide(inj, ProvideEncryptor)
		do.Provide(inj, ProvideEncryptionProvider)
	}
	if cfg.Redis != nil {
		do.Provide(inj, ProvideRedisCounterBackend)
	}
	if cfg.Cache.Enabled {
		do.Provide(inj, ProvideObjectCache)
	}
	if cfg.RateLimit.Enabled {
		do.Provide(inj, ProvideRateLimiter)
	}
	if cfg.UI.Enabled {
		do.Provide(inj, ProvideLoginThrottle)
		do.Provide(inj, ProvideUIHandler)
	}
	if cfg.UI.AdminKey != "" {
		do.Provide(inj, ProvideAdminHandler)
	}
	if len(cfg.Notifications.Endpoints) > 0 {
		do.Provide(inj, ProvideNotifier)
	}

	// --- Resolve core services (triggers lazy construction) ---
	db := do.MustInvoke[store.AdminStore](inj)
	cbStore := do.MustInvoke[*store.CircuitBreakerStore](inj)
	manager := do.MustInvoke[*proxy.BackendManager](inj)
	srv := do.MustInvoke[*s3api.Server](inj)

	// --- Store initial reloadable configs ---
	// These must be set BEFORE the lifecycle manager is constructed, because
	// service constructors (newReplicatorService, etc.) read worker intervals
	// from the config at creation time.
	manager.Rebalancer.SetConfig(&cfg.Rebalance)
	manager.Replicator.SetConfig(&cfg.Replication)
	manager.OverReplicationCleaner.SetConfig(&cfg.Replication)
	manager.SetUsageFlushConfig(&cfg.UsageFlush)
	manager.SetLifecycleConfig(&cfg.Lifecycle)
	manager.SetIntegrityConfig(&cfg.Integrity)

	sm := do.MustInvoke[*lifecycle.Manager](inj)

	// --- Store config in atomic pointer for safe SIGHUP access ---
	var cfgPtr syncutil.AtomicConfig[config.Config]
	cfgPtr.Store(cfg)

	// --- Initial quota metrics update ---
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to update initial quota metrics", "error", err)
	}

	// --- Start background services ---
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	bgDone := make(chan struct{})
	go func() {
		sm.Run(bgCtx)
		close(bgDone)
	}()

	// --- Build HTTP mux ---
	mux := http.NewServeMux()

	// Metrics endpoint
	var metricsServer *http.Server
	if cfg.Telemetry.Metrics.Enabled {
		if cfg.Telemetry.Metrics.Listen != "" {
			metricsMux := http.NewServeMux()
			metricsMux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
			metricsServer = &http.Server{
				Addr:    cfg.Telemetry.Metrics.Listen,
				Handler: metricsMux,
			}
			go func() {
				slog.InfoContext(ctx, "Metrics endpoint enabled on separate listener",
					"listen", cfg.Telemetry.Metrics.Listen, "path", cfg.Telemetry.Metrics.Path)
				if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					slog.ErrorContext(ctx, "Metrics listener failed", "error", err)
				}
			}()
		} else {
			mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
			slog.InfoContext(ctx, "Metrics endpoint enabled", "path", cfg.Telemetry.Metrics.Path)
		}
	}

	// Health endpoints
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if !cbStore.IsHealthy() {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not ready"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})

	// Rate limiter (optional, resolved from DI)
	var rl *s3api.RateLimiter
	if r, err := do.Invoke[*s3api.RateLimiter](inj); err == nil {
		rl = r
	}

	// Admin API (optional)
	if adminHandler, err := do.Invoke[*admin.Handler](inj); err == nil {
		adminMux := http.NewServeMux()
		adminHandler.Register(adminMux)
		var adminHTTP http.Handler = adminMux
		if rl != nil {
			adminHTTP = rl.Middleware(adminHTTP)
		}
		mux.Handle("/admin/", adminHTTP)
		slog.InfoContext(ctx, "Admin API enabled", "path", "/admin/api/")
	}

	// API-mode handlers: UI, S3 proxy
	var uiHandler *ui.Handler
	var loginThrottle *httputil.LoginThrottle
	if *mode == "api" || *mode == "all" {
		// Web UI dashboard (optional)
		if h, err := do.Invoke[*ui.Handler](inj); err == nil {
			uiHandler = h
			uiHandler.Register(mux, cfg.UI.Path)
			slog.InfoContext(ctx, "Web UI enabled", "path", cfg.UI.Path)
		}
		if lt, err := do.Invoke[*httputil.LoginThrottle](inj); err == nil {
			loginThrottle = lt
		}

		// S3 proxy handler with optional rate limiting and admission control
		var s3Handler http.Handler = srv
		if rl != nil {
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

	httpServer := &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	// --- Configure TLS ---
	var certReloader *httputil.CertReloader
	if cfg.Server.TLS.CertFile != "" {
		certReloader, err = httputil.NewCertReloader(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to load TLS certificate", "error", err)
			os.Exit(1)
		}

		tlsCfg := &tls.Config{
			GetCertificate: certReloader.GetCertificate,
			MinVersion:     parseTLSVersion(cfg.Server.TLS.MinVersion),
		}

		if cfg.Server.TLS.ClientCAFile != "" {
			caCert, err := os.ReadFile(cfg.Server.TLS.ClientCAFile)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to read client CA file", "error", err)
				os.Exit(1)
			}
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caCert) {
				slog.ErrorContext(ctx, "Failed to parse client CA certificate")
				os.Exit(1)
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			tlsCfg.ClientCAs = caPool
		}

		httpServer.TLSConfig = tlsCfg
	}

	// --- Handle SIGHUP for config reload ---
	hupChan := make(chan os.Signal, 1)
	hupDone := make(chan struct{})
	signal.Notify(hupChan, syscall.SIGHUP)
	go func() {
		defer close(hupDone)
		for range hupChan {
			slog.InfoContext(bgCtx, "SIGHUP received, reloading configuration", "path", *configPath)

			newCfg, err := config.LoadConfig(*configPath)
			if err != nil {
				slog.ErrorContext(bgCtx, "Config reload failed, keeping current config", "error", err)
				continue
			}

			currentCfg := cfgPtr.Load()
			if warnings := config.NonReloadableFieldsChanged(currentCfg, newCfg); len(warnings) > 0 {
				for _, w := range warnings {
					slog.WarnContext(bgCtx, "Config field changed but requires restart to take effect", "field", w)
				}
			}

			if certReloader != nil {
				if err := certReloader.Reload(); err != nil {
					slog.ErrorContext(bgCtx, "Failed to reload TLS certificate", "error", err)
				}
			}

			srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
			slog.InfoContext(bgCtx, "Reloaded bucket credentials", "buckets", len(newCfg.Buckets))

			if rl != nil && newCfg.RateLimit.Enabled {
				rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
			}

			reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := db.SyncQuotaLimits(reloadCtx, newCfg.Backends); err != nil {
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
			manager.UpdateUsageLimits(newUsageLimits)

			logLevel.Set(config.ParseLogLevel(newCfg.Server.LogLevel))

			manager.Rebalancer.SetConfig(&newCfg.Rebalance)
			manager.Replicator.SetConfig(&newCfg.Replication)
			manager.OverReplicationCleaner.SetConfig(&newCfg.Replication)
			manager.SetUsageFlushConfig(&newCfg.UsageFlush)
			manager.SetLifecycleConfig(&newCfg.Lifecycle)
			manager.SetIntegrityConfig(&newCfg.Integrity)

			if err := manager.UpdateQuotaMetrics(reloadCtx); err != nil {
				slog.WarnContext(reloadCtx, "Failed to update quota metrics after reload", "error", err)
			}
			reloadCancel()

			if uiHandler != nil {
				uiHandler.UpdateConfig(newCfg)
			}

			cfgPtr.Store(newCfg)
			slog.InfoContext(bgCtx, "Configuration reload complete")
		}
	}()

	// --- Handle graceful shutdown ---
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan

		slog.InfoContext(ctx, "Shutting down", "signal", sig.String())

		if delay := cfgPtr.Load().Server.ShutdownDelay; delay > 0 {
			slog.InfoContext(ctx, "Waiting for load balancer deregistration", "delay", delay)
			time.Sleep(delay)
		}

		ready.Store(false)

		signal.Stop(hupChan)
		close(hupChan)
		<-hupDone

		// Use a single 30-second root deadline for the entire shutdown
		// sequence so total time is bounded regardless of how many phases
		// there are.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "HTTP server shutdown error", "error", err)
		}

		if metricsServer != nil {
			if err := metricsServer.Shutdown(shutdownCtx); err != nil {
				slog.ErrorContext(ctx, "Metrics server shutdown error", "error", err)
			}
		}

		if rl != nil {
			rl.Close()
		}
		if loginThrottle != nil {
			loginThrottle.Close()
		}

		bgCancel()
		<-bgDone
		sm.Stop(10 * time.Second)

		// Stop Vault token renewal goroutine (if active)
		if encProvider, err := do.Invoke[encryption.KeyProvider](inj); err == nil {
			if closer, ok := encProvider.(interface{ Close() }); ok {
				closer.Close()
			}
		}

		manager.Close()

		if err := manager.FlushUsage(shutdownCtx); err != nil {
			slog.WarnContext(shutdownCtx, "Final usage flush failed", "error", err)
		}

		if redisBackend, err := do.Invoke[*counter.RedisCounterBackend](inj); err == nil {
			if err := redisBackend.Close(); err != nil {
				slog.WarnContext(shutdownCtx, "Failed to close Redis client", "error", err)
			}
		}

		db.Close()

		if err := shutdownTracer(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Tracer shutdown error", "error", err)
		}
	}()

	// --- Mark service as ready ---
	ready.Store(true)

	// --- Log startup info ---
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}
	slog.InfoContext(ctx, "S3 Orchestrator starting",
		"version", telemetry.Version,
		"mode", *mode,
		"listen_addr", cfg.Server.ListenAddr,
		"log_level", cfg.Server.LogLevel,
		"buckets", bucketNames,
		"backends", len(cfg.Backends),
		"routing_strategy", cfg.RoutingStrategy,
	)

	// --- Start server ---
	if httpServer.TLSConfig != nil {
		err = httpServer.ListenAndServeTLS("", "")
	} else {
		err = httpServer.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		slog.ErrorContext(ctx, "Server error", "error", err)
		os.Exit(1)
	}

	<-shutdownDone
	slog.InfoContext(ctx, "Server stopped")
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
