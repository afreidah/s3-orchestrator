// -------------------------------------------------------------------------------
// S3 Orchestrator - Unified S3 Endpoint with Quota Management
//
// Author: Alex Freidah
//
// Entry point for the S3 proxy service. Dispatches to subcommands: "serve"
// (default) starts the HTTP server, "sync" imports pre-existing bucket objects
// into the proxy's metadata database.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/server"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ui"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sync" {
		os.Args = os.Args[1:]
		runSync()
		return
	}
	runServe()
}

func runServe() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// --- Initialize structured logger ---
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// --- Load configuration ---
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// --- Initialize tracing ---
	ctx := context.Background()
	shutdownTracer, err := telemetry.InitTracer(ctx, cfg.Telemetry.Tracing)
	if err != nil {
		slog.Error("Failed to initialize tracer", "error", err)
		os.Exit(1)
	}

	// --- Set build info metric ---
	telemetry.BuildInfo.WithLabelValues(telemetry.Version, runtime.Version()).Set(1)

	// --- Initialize PostgreSQL store ---
	store, err := storage.NewStore(ctx, &cfg.Database)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	slog.Info("Connected to PostgreSQL",
		"host", cfg.Database.Host,
		"port", cfg.Database.Port,
		"database", cfg.Database.Database,
	)

	// --- Run database migrations ---
	if err := store.RunMigrations(ctx); err != nil {
		slog.Error("Failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("Database migrations applied")

	// --- Sync quota limits from config to database ---
	if err := store.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		slog.Error("Failed to sync quota limits", "error", err)
		os.Exit(1)
	}

	// --- Initialize backends ---
	backends := make(map[string]storage.ObjectBackend)
	backendOrder := make([]string, 0, len(cfg.Backends))

	usageLimits := make(map[string]storage.UsageLimits, len(cfg.Backends))
	for i := range cfg.Backends {
		bcfg := &cfg.Backends[i]
		backend, err := storage.NewS3Backend(bcfg)
		if err != nil {
			slog.Error("Failed to initialize backend", "backend", bcfg.Name, "error", err)
			os.Exit(1)
		}
		backends[bcfg.Name] = backend
		backendOrder = append(backendOrder, bcfg.Name)
		usageLimits[bcfg.Name] = storage.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
		slog.Info("Backend initialized",
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
	cbStore := storage.NewCircuitBreakerStore(store, cfg.CircuitBreaker)

	// --- Create backend manager ---
	manager := storage.NewBackendManager(&storage.BackendManagerConfig{
		Backends:          backends,
		Store:             cbStore,
		Order:             backendOrder,
		CacheTTL:          cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:    cfg.Server.BackendTimeout,
		UsageLimits:       usageLimits,
		RoutingStrategy:   cfg.RoutingStrategy,
		ParallelBroadcast: cfg.CircuitBreaker.ParallelBroadcast,
	})

	// --- Store initial reloadable configs ---
	manager.SetRebalanceConfig(&cfg.Rebalance)
	manager.SetReplicationConfig(&cfg.Replication)
	manager.SetUsageFlushConfig(&cfg.UsageFlush)
	manager.SetLifecycleConfig(&cfg.Lifecycle)

	// --- Initial quota metrics update ---
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.Warn("Failed to update initial quota metrics", "error", err)
	}

	// --- Start background services with lifecycle manager ---
	sm := lifecycle.NewManager()
	sm.Register("usage-flush", &usageFlushService{manager: manager})
	sm.Register("multipart-cleanup", &multipartCleanupService{manager: manager, store: cbStore})
	sm.Register("cleanup-queue", &cleanupQueueService{manager: manager, store: cbStore})
	sm.Register("rebalancer", &rebalancerService{manager: manager, store: cbStore})
	sm.Register("replicator", &replicatorService{manager: manager, store: cbStore})
	sm.Register("lifecycle", &lifecycleService{manager: manager, store: cbStore})

	if cfg.Rebalance.Enabled {
		slog.Info("Rebalancer enabled",
			"strategy", cfg.Rebalance.Strategy,
			"interval", cfg.Rebalance.Interval,
			"batch_size", cfg.Rebalance.BatchSize,
			"threshold", cfg.Rebalance.Threshold,
		)
	}
	if cfg.Replication.Factor > 1 {
		slog.Info("Replication worker enabled",
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
	srv := &server.Server{
		Manager:       manager,
		MaxObjectSize: cfg.Server.MaxObjectSize,
	}
	srv.SetBucketAuth(bucketAuth)

	// --- Setup HTTP mux ---
	mux := http.NewServeMux()

	// Metrics endpoint
	if cfg.Telemetry.Metrics.Enabled {
		mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		slog.Info("Metrics endpoint enabled", "path", cfg.Telemetry.Metrics.Path)
	}

	// Health check endpoint — always 200 so service stays in Consul rotation.
	// Body reflects DB state for monitoring.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if cbStore.IsHealthy() {
			_, _ = w.Write([]byte("ok"))
		} else {
			_, _ = w.Write([]byte("degraded"))
		}
	})

	// Web UI dashboard
	var uiHandler *ui.Handler
	if cfg.UI.Enabled {
		uiHandler = ui.New(manager, cbStore.IsHealthy, cfg)
		uiHandler.Register(mux, cfg.UI.Path)
		slog.Info("Web UI enabled", "path", cfg.UI.Path)
	}

	// S3 proxy handler (all other paths), optionally rate-limited
	var rl *server.RateLimiter
	var s3Handler http.Handler = srv
	if cfg.RateLimit.Enabled {
		rl = server.NewRateLimiter(cfg.RateLimit)
		s3Handler = rl.Middleware(srv)
		slog.Info("Rate limiting enabled",
			"requests_per_sec", cfg.RateLimit.RequestsPerSec,
			"burst", cfg.RateLimit.Burst,
		)
	}
	mux.Handle("/", s3Handler)

	httpServer := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	// --- Configure TLS if cert and key are provided ---
	var certReloader *server.CertReloader
	if cfg.Server.TLS.CertFile != "" {
		certReloader, err = server.NewCertReloader(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		if err != nil {
			slog.Error("Failed to load TLS certificate", "error", err)
			os.Exit(1)
		}

		tlsCfg := &tls.Config{
			GetCertificate: certReloader.GetCertificate,
			MinVersion:     parseTLSVersion(cfg.Server.TLS.MinVersion),
		}

		if cfg.Server.TLS.ClientCAFile != "" {
			caCert, err := os.ReadFile(cfg.Server.TLS.ClientCAFile)
			if err != nil {
				slog.Error("Failed to read client CA file", "error", err)
				os.Exit(1)
			}
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caCert) {
				slog.Error("Failed to parse client CA certificate")
				os.Exit(1)
			}
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			tlsCfg.ClientCAs = caPool
		}

		httpServer.TLSConfig = tlsCfg
	}

	// --- Handle SIGHUP for config reload ---
	hupChan := make(chan os.Signal, 1)
	signal.Notify(hupChan, syscall.SIGHUP)
	go func() {
		for range hupChan {
			slog.Info("SIGHUP received, reloading configuration", "path", *configPath)

			newCfg, err := config.LoadConfig(*configPath)
			if err != nil {
				slog.Error("Config reload failed, keeping current config", "error", err)
				continue
			}

			// Warn about non-reloadable changes
			if warnings := config.NonReloadableFieldsChanged(cfg, newCfg); len(warnings) > 0 {
				for _, w := range warnings {
					slog.Warn("Config field changed but requires restart to take effect", "field", w)
				}
			}

			// Reload TLS certificate from disk
			if certReloader != nil {
				if err := certReloader.Reload(); err != nil {
					slog.Error("Failed to reload TLS certificate", "error", err)
				}
			}

			// Reload bucket credentials
			srv.SetBucketAuth(auth.NewBucketRegistry(newCfg.Buckets))
			slog.Info("Reloaded bucket credentials", "buckets", len(newCfg.Buckets))

			// Reload rate limiter settings
			if rl != nil && newCfg.RateLimit.Enabled {
				rl.UpdateLimits(newCfg.RateLimit.RequestsPerSec, newCfg.RateLimit.Burst)
				slog.Info("Reloaded rate limits",
					"requests_per_sec", newCfg.RateLimit.RequestsPerSec,
					"burst", newCfg.RateLimit.Burst,
				)
			}

			// Reload quota limits in database
			if err := store.SyncQuotaLimits(bgCtx, newCfg.Backends); err != nil {
				slog.Error("Failed to sync quota limits on reload", "error", err)
			} else {
				slog.Info("Reloaded backend quota limits")
			}

			// Reload usage limits
			newUsageLimits := make(map[string]storage.UsageLimits, len(newCfg.Backends))
			for i := range newCfg.Backends {
				bcfg := &newCfg.Backends[i]
				newUsageLimits[bcfg.Name] = storage.UsageLimits{
					APIRequestLimit:  bcfg.APIRequestLimit,
					EgressByteLimit:  bcfg.EgressByteLimit,
					IngressByteLimit: bcfg.IngressByteLimit,
				}
			}
			manager.UpdateUsageLimits(newUsageLimits)
			slog.Info("Reloaded backend usage limits")

			// Reload rebalance/replication/usage-flush/lifecycle config
			manager.SetRebalanceConfig(&newCfg.Rebalance)
			manager.SetReplicationConfig(&newCfg.Replication)
			manager.SetUsageFlushConfig(&newCfg.UsageFlush)
			manager.SetLifecycleConfig(&newCfg.Lifecycle)
			slog.Info("Reloaded rebalance/replication/usage-flush/lifecycle config")

			// Update quota metrics with new limits
			if err := manager.UpdateQuotaMetrics(bgCtx); err != nil {
				slog.Warn("Failed to update quota metrics after reload", "error", err)
			}

			// Update dashboard config
			if uiHandler != nil {
				uiHandler.UpdateConfig(newCfg)
			}

			cfg = newCfg
			slog.Info("Configuration reload complete")
		}
	}()

	// --- Handle graceful shutdown ---
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		slog.Info("Shutting down")

		// Stop SIGHUP handler so it can't race with shutdown
		signal.Stop(hupChan)
		close(hupChan)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Drain inflight HTTP requests first so clients get responses quickly
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP server shutdown error", "error", err)
		}

		// Stop rate limiter cleanup goroutine
		if rl != nil {
			rl.Close()
		}

		// Stop background services and wait for them to finish
		bgCancel()
		<-bgDone
		sm.Stop(10 * time.Second)

		// Stop cache eviction goroutine
		manager.Close()

		// Flush usage counters before closing database
		if err := manager.FlushUsage(shutdownCtx); err != nil {
			slog.Warn("Failed to flush usage counters on shutdown", "error", err)
		}

		// Close database connection
		store.Close()

		// Flush traces
		if err := shutdownTracer(shutdownCtx); err != nil {
			slog.Error("Tracer shutdown error", "error", err)
		}
	}()

	// --- Log startup info ---
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}
	slog.Info("S3 Orchestrator starting",
		"version", telemetry.Version,
		"listen_addr", cfg.Server.ListenAddr,
		"buckets", bucketNames,
		"backends", len(cfg.Backends),
		"routing_strategy", cfg.RoutingStrategy,
	)

	if cfg.Telemetry.Tracing.Enabled {
		slog.Info("Tracing enabled",
			"endpoint", cfg.Telemetry.Tracing.Endpoint,
			"sample_rate", cfg.Telemetry.Tracing.SampleRate,
			"insecure", cfg.Telemetry.Tracing.Insecure,
		)
	}

	if cfg.Server.TLS.CertFile != "" {
		slog.Info("TLS enabled",
			"cert_file", cfg.Server.TLS.CertFile,
			"min_version", cfg.Server.TLS.MinVersion,
			"mtls", cfg.Server.TLS.ClientCAFile != "",
		)
	}

	if cfg.CircuitBreaker.ParallelBroadcast {
		slog.Info("Parallel broadcast reads enabled for degraded mode")
	}

	if len(cfg.Lifecycle.Rules) > 0 {
		slog.Info("Lifecycle rules enabled", "rules", len(cfg.Lifecycle.Rules))
	}

	// --- Start server ---
	if httpServer.TLSConfig != nil {
		err = httpServer.ListenAndServeTLS("", "") // certs provided via GetCertificate
	} else {
		err = httpServer.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		slog.Error("Server error", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown goroutine to finish cleanup
	<-shutdownDone

	slog.Info("Server stopped")
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
