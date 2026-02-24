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
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
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
		Backends:        backends,
		Store:           cbStore,
		Order:           backendOrder,
		CacheTTL:        cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:  cfg.Server.BackendTimeout,
		UsageLimits:     usageLimits,
		RoutingStrategy: cfg.RoutingStrategy,
	})

	// --- Store initial reloadable configs ---
	manager.SetRebalanceConfig(&cfg.Rebalance)
	manager.SetReplicationConfig(&cfg.Replication)

	// --- Initial quota metrics update ---
	if err := manager.UpdateQuotaMetrics(ctx); err != nil {
		slog.Warn("Failed to update initial quota metrics", "error", err)
	}

	// --- Start background tasks with cancellable context ---
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	var bgWg sync.WaitGroup

	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := manager.FlushUsage(bgCtx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
					slog.Error("Failed to flush usage counters", "error", err)
				}
				if err := manager.UpdateQuotaMetrics(bgCtx); err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
					slog.Error("Failed to update quota metrics", "error", err)
				}
			case <-bgCtx.Done():
				return
			}
		}
	}()

	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				manager.CleanupStaleMultipartUploads(bgCtx, 24*time.Hour)
			case <-bgCtx.Done():
				return
			}
		}
	}()

	// --- Rebalancer background task (always runs, skips when disabled) ---
	if cfg.Rebalance.Enabled {
		slog.Info("Rebalancer enabled",
			"strategy", cfg.Rebalance.Strategy,
			"interval", cfg.Rebalance.Interval,
			"batch_size", cfg.Rebalance.BatchSize,
			"threshold", cfg.Rebalance.Threshold,
		)
	}
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rcfg := manager.RebalanceConfig()
				if rcfg == nil || !rcfg.Enabled {
					continue
				}
				moved, err := manager.Rebalance(bgCtx, *rcfg)
				if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
					slog.Error("Rebalance failed", "error", err)
				} else if moved > 0 {
					slog.Info("Rebalance completed", "objects_moved", moved)
					_ = manager.UpdateQuotaMetrics(bgCtx)
				}
			case <-bgCtx.Done():
				return
			}
		}
	}()

	// --- Replication worker background task (always runs, skips when disabled) ---
	if cfg.Replication.Factor > 1 {
		slog.Info("Replication worker enabled",
			"factor", cfg.Replication.Factor,
			"interval", cfg.Replication.WorkerInterval,
			"batch_size", cfg.Replication.BatchSize,
		)
	}
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()

		// Run once at startup to catch up on pending replicas
		rcfg := manager.ReplicationConfig()
		if rcfg != nil && rcfg.Factor > 1 {
			created, err := manager.Replicate(bgCtx, *rcfg)
			if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
				slog.Error("Replication startup run failed", "error", err)
			} else if created > 0 {
				slog.Info("Replication startup run completed", "copies_created", created)
				_ = manager.UpdateQuotaMetrics(bgCtx)
			}
		}

		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rcfg := manager.ReplicationConfig()
				if rcfg == nil || rcfg.Factor <= 1 {
					continue
				}
				created, err := manager.Replicate(bgCtx, *rcfg)
				if err != nil && !errors.Is(err, storage.ErrDBUnavailable) {
					slog.Error("Replication failed", "error", err)
				} else if created > 0 {
					slog.Info("Replication completed", "copies_created", created)
					_ = manager.UpdateQuotaMetrics(bgCtx)
				}
			case <-bgCtx.Done():
				return
			}
		}
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

			// Reload rebalance/replication config
			manager.SetRebalanceConfig(&newCfg.Rebalance)
			manager.SetReplicationConfig(&newCfg.Replication)
			slog.Info("Reloaded rebalance/replication config")

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

		// Stop background goroutines and wait for them to finish
		bgCancel()
		bgWg.Wait()

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

	// --- Start server ---
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("Server error", "error", err)
		os.Exit(1)
	}

	// Wait for shutdown goroutine to finish cleanup
	<-shutdownDone

	slog.Info("Server stopped")
}
