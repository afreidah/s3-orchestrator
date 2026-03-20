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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/afreidah/s3-orchestrator/internal/admin"
	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/httputil"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/server"

	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ui"

	// codecov:ignore -- process entry point, delegates to tested functions
	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/worker"
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

	// --- Instance ID for health responses ---
	instanceID, _ := os.Hostname()
	if instanceID == "" {
		instanceID = "unknown"
	}

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
	audit.OnEvent = func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	}

	// --- Initialize PostgreSQL store ---
	db, err := store.NewStore(ctx, &cfg.Database)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to connect to database", "error", err)
		os.Exit(1)
	}
	slog.InfoContext(ctx, "Connected to PostgreSQL",
		"host", cfg.Database.Host,
		"port", cfg.Database.Port,
		"database", cfg.Database.Database,
	)

	// --- Run database migrations ---
	if err := db.RunMigrations(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.InfoContext(ctx, "Database migrations applied")

	// --- Sync quota limits from config to database ---
	if err := db.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		slog.ErrorContext(ctx, "Failed to sync quota limits", "error", err)
		os.Exit(1)
	}

	// --- Initialize backends ---
	backends := make(map[string]backend.ObjectBackend)
	backendOrder := make([]string, 0, len(cfg.Backends))

	usageLimits := make(map[string]store.UsageLimits, len(cfg.Backends))
	for i := range cfg.Backends {
		bcfg := &cfg.Backends[i]
		s3Backend, err := backend.NewS3Backend(bcfg)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to initialize backend", "backend", bcfg.Name, "error", err)
			os.Exit(1)
		}
		var be backend.ObjectBackend = s3Backend
		if cfg.BackendCircuitBreaker.Enabled {
			be = backend.NewCircuitBreakerBackend(s3Backend, bcfg.Name,
				cfg.BackendCircuitBreaker.FailureThreshold,
				cfg.BackendCircuitBreaker.OpenTimeout)
		}
		backends[bcfg.Name] = be
		backendOrder = append(backendOrder, bcfg.Name)
		usageLimits[bcfg.Name] = store.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
		slog.InfoContext(ctx, "Backend initialized",
			"backend", bcfg.Name,
			"endpoint", bcfg.Endpoint,
			"bucket", bcfg.Bucket,
			"quota_bytes", bcfg.QuotaBytes,
			"api_request_limit", bcfg.APIRequestLimit,
			"egress_byte_limit", bcfg.EgressByteLimit,
			"ingress_byte_limit", bcfg.IngressByteLimit,
		)
	}

	// --- Wrap store with circuit breaker for runtime ---
	cbStore := store.NewCircuitBreakerStore(db, cfg.CircuitBreaker)

	// --- Initialize encryption (if enabled) ---
	var encryptor *encryption.Encryptor
	var encryptionProvider encryption.KeyProvider
	if cfg.Encryption.Enabled {
		provider, err := encryption.NewKeyProviderFromConfig(&cfg.Encryption)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to initialize encryption key provider", "error", err)
			os.Exit(1)
		}
		encryptionProvider = provider
		encryptor = encryption.NewEncryptor(provider, cfg.Encryption.ChunkSize)
		slog.InfoContext(ctx, "Server-side encryption enabled",
			"chunk_size", cfg.Encryption.ChunkSize,
			"key_id", provider.KeyID(),
		)
	}

	// --- Initialize shared counter backend (Redis or local) ---
	var counterBackend counter.CounterBackend
	var redisBackend *counter.RedisCounterBackend
	if cfg.Redis != nil {
		redisOpts := &redis.Options{
			Addr:     cfg.Redis.Address,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		}
		if cfg.Redis.TLS {
			redisOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		redisClient := redis.NewClient(redisOpts)
		var err error
		redisBackend, err = counter.NewRedisCounterBackend(redisClient, cfg.Redis, backendOrder)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to connect to Redis", "error", err)
			os.Exit(1)
		}
		counterBackend = redisBackend
		slog.InfoContext(ctx, "Redis shared counters enabled", "address", cfg.Redis.Address)
	}

	// --- Create admission semaphore ---
	// Shared between the HTTP admission controller and background services
	// so both draw from one concurrency budget.
	var admissionSem chan struct{}
	if cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0 {
		// Split pools are handled at the HTTP layer; background services use
		// the write pool. Create combined for the manager; the admission
		// controller will use split semaphores from main.
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentWrites)
	} else if cfg.Server.MaxConcurrentRequests > 0 {
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentRequests)
	}

	// --- Create backend manager ---
	manager := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:           backends,
		Store:              cbStore,
		Order:              backendOrder,
		CacheTTL:           cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:     cfg.Server.BackendTimeout,
		UsageLimits:        usageLimits,
		RoutingStrategy:    cfg.RoutingStrategy,
		ParallelBroadcast:  cfg.CircuitBreaker.ParallelBroadcast,
		Encryptor:          encryptor,
		CounterBackend:     counterBackend,
		CleanupConcurrency: cfg.CleanupQueue.Concurrency,
		AdmissionSem:       admissionSem,
	})

	// --- Store config in atomic pointer for safe SIGHUP access ---
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(cfg)

	// --- Store initial reloadable configs ---
	manager.Rebalancer.SetConfig(&cfg.Rebalance)
	manager.Replicator.SetConfig(&cfg.Replication)
	manager.OverReplicationCleaner.SetConfig(&cfg.Replication)
	manager.SetUsageFlushConfig(&cfg.UsageFlush)
	manager.SetLifecycleConfig(&cfg.Lifecycle)

	// --- Initial quota metrics update ---
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to update initial quota metrics", "error", err)
	}

	// --- Start background services with lifecycle manager ---
	sm := lifecycle.NewManager()
	sm.Register("usage-flush", &usageFlushService{manager: manager, locker: cbStore}) // all modes — data safety

	if *mode == "worker" || *mode == "all" {
		sm.Register("multipart-cleanup", newMultipartCleanupService(manager, cbStore))
		sm.Register("cleanup-queue", newCleanupQueueService(manager, cbStore))
		sm.Register("rebalancer", newRebalancerService(manager, cbStore))
		sm.Register("replicator", newReplicatorService(manager, cbStore))
		sm.Register("over-replication", newOverReplicationService(manager, cbStore))
		sm.Register("lifecycle", newLifecycleService(manager, cbStore))

		if cfg.Reconcile.Enabled {
			bktNames := make([]string, len(cfg.Buckets))
			for i, b := range cfg.Buckets {
				bktNames[i] = b.Name
			}
			reconciler := worker.NewReconciler(manager, bktNames)
			sm.Register("reconcile", newReconcileService(reconciler, cbStore, cfg.Reconcile.Interval))
			slog.InfoContext(ctx, "Reconciler enabled", "interval", cfg.Reconcile.Interval)
		}
	}

	if cfg.Rebalance.Enabled {
		slog.InfoContext(ctx, "Rebalancer enabled",
			"strategy", cfg.Rebalance.Strategy,
			"interval", cfg.Rebalance.Interval,
			"batch_size", cfg.Rebalance.BatchSize,
			"threshold", cfg.Rebalance.Threshold,
		)
	}
	if cfg.Replication.Factor > 1 {
		slog.InfoContext(ctx, "Replication worker enabled",
			"factor", cfg.Replication.Factor,
			"interval", cfg.Replication.WorkerInterval,
			"batch_size", cfg.Replication.BatchSize,
		)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	bgDone := make(chan struct{})
	go func() {
		sm.Run(bgCtx)
		close(bgDone)
	}()

	// --- Build bucket registry ---
	bucketAuth := auth.NewBucketRegistry(cfg.Buckets)

	// --- Create server ---
	srv := server.NewServer(manager, cfg.Server.MaxObjectSize)
	srv.SetBucketAuth(bucketAuth)

	// --- Setup HTTP mux ---
	mux := http.NewServeMux()

	// Metrics endpoint
	if cfg.Telemetry.Metrics.Enabled {
		mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		slog.InfoContext(ctx, "Metrics endpoint enabled", "path", cfg.Telemetry.Metrics.Path)
	}

	// Liveness endpoint — always 200 so the service stays in Consul/K8s rotation.
	// Body reflects DB state for monitoring; instance ID aids multi-instance debugging.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if !cbStore.IsHealthy() {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":%q,"instance":%q}`, status, instanceID)
	})

	// Readiness endpoint — returns 503 until startup completes and during shutdown drain.
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"status":"not ready","instance":%q}`, instanceID)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ready","instance":%q}`, instanceID)
	})

	// --- Rate limiter (shared by S3 proxy and admin API) ---
	var uiHandler *ui.Handler
	var rl *server.RateLimiter
	var loginThrottle *httputil.LoginThrottle

	if cfg.RateLimit.Enabled {
		rl = server.NewRateLimiter(cfg.RateLimit)
		slog.InfoContext(ctx, "Rate limiting enabled",
			"requests_per_sec", cfg.RateLimit.RequestsPerSec,
			"burst", cfg.RateLimit.Burst,
		)
	}

	// --- Admin API (all modes — operators need it everywhere) ---
	if cfg.UI.AdminKey != "" {
		adminToken := cfg.UI.AdminToken
		if adminToken == "" {
			adminToken = cfg.UI.AdminKey
		}
		adminMux := http.NewServeMux()
		adminHandler := admin.New(manager, cbStore, db, encryptor, adminToken, &logLevel)
		adminHandler.Register(adminMux)
		var adminHTTP http.Handler = adminMux
		if rl != nil {
			adminHTTP = rl.Middleware(adminHTTP)
		}
		mux.Handle("/admin/", adminHTTP)
		slog.InfoContext(ctx, "Admin API enabled", "path", "/admin/api/")
	}

	// --- API-mode handlers: UI, S3 proxy ---
	if *mode == "api" || *mode == "all" {
		// Web UI dashboard
		if cfg.UI.Enabled {
			loginThrottle = httputil.NewLoginThrottle(5, 5*time.Minute)
			uiHandler = ui.New(manager, cbStore.IsHealthy, cfg, logBuffer, loginThrottle)
			uiHandler.Register(mux, cfg.UI.Path)
			slog.InfoContext(ctx, "Web UI enabled", "path", cfg.UI.Path)
		}

		// S3 proxy handler (all other paths), optionally rate-limited and admission-controlled
		var s3Handler http.Handler = srv
		if rl != nil {
			s3Handler = rl.Middleware(s3Handler)
		}
		if cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0 {
			readSem := make(chan struct{}, cfg.Server.MaxConcurrentReads)
			ac := server.NewSplitAdmissionControllerFromSem(readSem, admissionSem)
			if cfg.Server.LoadShedThreshold > 0 {
				ac.SetShedThreshold(cfg.Server.LoadShedThreshold)
			}
			if cfg.Server.AdmissionWait > 0 {
				ac.SetAdmissionWait(cfg.Server.AdmissionWait)
			}
			s3Handler = ac.Middleware(s3Handler)
			slog.InfoContext(ctx, "Split admission control enabled",
				"max_concurrent_reads", cfg.Server.MaxConcurrentReads,
				"max_concurrent_writes", cfg.Server.MaxConcurrentWrites,
				"load_shed_threshold", cfg.Server.LoadShedThreshold,
				"admission_wait", cfg.Server.AdmissionWait,
			)
		} else if cfg.Server.MaxConcurrentRequests > 0 {
			ac := server.NewAdmissionControllerFromSem(admissionSem)
			if cfg.Server.LoadShedThreshold > 0 {
				ac.SetShedThreshold(cfg.Server.LoadShedThreshold)
			}
			if cfg.Server.AdmissionWait > 0 {
				ac.SetAdmissionWait(cfg.Server.AdmissionWait)
			}
			s3Handler = ac.Middleware(s3Handler)
			slog.InfoContext(ctx, "Admission control enabled",
				"max_concurrent_requests", cfg.Server.MaxConcurrentRequests,
				"load_shed_threshold", cfg.Server.LoadShedThreshold,
				"admission_wait", cfg.Server.AdmissionWait,
			)
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

	// --- Configure TLS if cert and key are provided ---
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

			// Warn about non-reloadable changes
			currentCfg := cfgPtr.Load()
			if warnings := config.NonReloadableFieldsChanged(currentCfg, newCfg); len(warnings) > 0 {
				for _, w := range warnings {
					slog.WarnContext(bgCtx, "Config field changed but requires restart to take effect", "field", w)
				}
			}

			// Reload TLS certificate from disk
			if certReloader != nil {
				if err := certReloader.Reload(); err != nil {
					slog.ErrorContext(bgCtx, "Failed to reload TLS certificate", "error", err)
				}
			}

			// Reload bucket credentials
			srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
			slog.InfoContext(bgCtx, "Reloaded bucket credentials", "buckets", len(newCfg.Buckets))

			// Reload rate limiter settings
			if rl != nil && newCfg.RateLimit.Enabled {
				rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
				slog.InfoContext(bgCtx, "Reloaded rate limits",
					"requests_per_sec", newCfg.RateLimit.RequestsPerSec,
					"burst", newCfg.RateLimit.Burst,
				)
			}

			// Reload quota limits in database (dedicated timeout, not bgCtx)
			reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := db.SyncQuotaLimits(reloadCtx, newCfg.Backends); err != nil {
				slog.ErrorContext(reloadCtx, "Failed to sync quota limits on reload", "error", err)
			} else {
				slog.InfoContext(reloadCtx, "Reloaded backend quota limits")
			}

			// Reload usage limits
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
			slog.InfoContext(reloadCtx, "Reloaded backend usage limits")

			// Reload log level
			logLevel.Set(config.ParseLogLevel(newCfg.Server.LogLevel))
			slog.InfoContext(reloadCtx, "Reloaded log level", "level", newCfg.Server.LogLevel)

			// Reload rebalance/replication/usage-flush/lifecycle config
			manager.Rebalancer.SetConfig(&newCfg.Rebalance)
			manager.Replicator.SetConfig(&newCfg.Replication)
			manager.OverReplicationCleaner.SetConfig(&newCfg.Replication)
			manager.SetUsageFlushConfig(&newCfg.UsageFlush)
			manager.SetLifecycleConfig(&newCfg.Lifecycle)
			slog.InfoContext(reloadCtx, "Reloaded rebalance/replication/usage-flush/lifecycle config")

			// Update quota metrics with new limits
			if err := manager.UpdateQuotaMetrics(reloadCtx); err != nil {
				slog.WarnContext(reloadCtx, "Failed to update quota metrics after reload", "error", err)
			}
			reloadCancel()

			// Update dashboard config
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

		// Optional pre-stop delay for async LB deregistration (Consul, K8s).
		// The delay runs before toggling readiness off so the LB continues to
		// see the service as healthy while it finishes deregistering.
		if delay := cfgPtr.Load().Server.ShutdownDelay; delay > 0 {
			slog.InfoContext(ctx, "Waiting for load balancer deregistration", "delay", delay)
			time.Sleep(delay)
		}

		// Toggle readiness off so load balancers stop routing new traffic
		ready.Store(false)

		// Stop SIGHUP handler so it can't race with shutdown
		signal.Stop(hupChan)
		close(hupChan)
		<-hupDone

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Drain inflight HTTP requests first so clients get responses quickly
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "HTTP server shutdown error", "error", err)
		}

		// Stop rate limiter and login throttle cleanup goroutines
		if rl != nil {
			rl.Close()
		}
		if loginThrottle != nil {
			loginThrottle.Close()
		}

		// Flush usage counters while services are still running so in-flight
		// increments are captured.
		preFlushCtx, preFlushCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := manager.FlushUsage(preFlushCtx); err != nil {
			slog.WarnContext(preFlushCtx, "Pre-shutdown usage flush failed", "error", err)
		}
		preFlushCancel()

		// Stop background services and wait for them to finish
		bgCancel()
		<-bgDone
		sm.Stop(10 * time.Second)

		// Stop Vault token renewal goroutine (if active)
		if closer, ok := encryptionProvider.(interface{ Close() }); ok {
			closer.Close()
		}

		// Stop cache eviction goroutine
		manager.Close()

		// Final flush captures any deltas from the bgCancel→bgDone window.
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer flushCancel()
		if err := manager.FlushUsage(flushCtx); err != nil {
			slog.WarnContext(flushCtx, "Final usage flush failed", "error", err)
		}

		// Close Redis client (after flush so counters are flushed to PG first)
		if redisBackend != nil {
			if err := redisBackend.Close(); err != nil {
				slog.WarnContext(flushCtx, "Failed to close Redis client", "error", err)
			}
		}

		// Close database connection
		db.Close()

		// Flush traces
		traceCtx, traceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer traceCancel()
		if err := shutdownTracer(traceCtx); err != nil {
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

	if cfg.Telemetry.Tracing.Enabled {
		slog.InfoContext(ctx, "Tracing enabled",
			"endpoint", cfg.Telemetry.Tracing.Endpoint,
			"sample_rate", cfg.Telemetry.Tracing.SampleRate,
			"insecure", cfg.Telemetry.Tracing.Insecure,
		)
	}

	if cfg.Server.TLS.CertFile != "" {
		slog.InfoContext(ctx, "TLS enabled",
			"cert_file", cfg.Server.TLS.CertFile,
			"min_version", cfg.Server.TLS.MinVersion,
			"mtls", cfg.Server.TLS.ClientCAFile != "",
		)
	}

	if cfg.BackendCircuitBreaker.Enabled {
		slog.InfoContext(ctx, "Backend circuit breakers enabled",
			"failure_threshold", cfg.BackendCircuitBreaker.FailureThreshold,
			"open_timeout", cfg.BackendCircuitBreaker.OpenTimeout,
		)
	}

	if cfg.Encryption.Enabled {
		slog.InfoContext(ctx, "Server-side encryption active",
			"chunk_size", cfg.Encryption.ChunkSize,
		)
	}

	if cfg.CircuitBreaker.ParallelBroadcast {
		slog.InfoContext(ctx, "Parallel broadcast reads enabled for degraded mode")
	}

	if len(cfg.Lifecycle.Rules) > 0 {
		slog.InfoContext(ctx, "Lifecycle rules enabled", "rules", len(cfg.Lifecycle.Rules))
	}

	// --- Start server ---
	if httpServer.TLSConfig != nil {
		err = httpServer.ListenAndServeTLS("", "") // certs provided via GetCertificate
	} else {
		err = httpServer.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		slog.ErrorContext(ctx, "Server error", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown goroutine to finish cleanup
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
