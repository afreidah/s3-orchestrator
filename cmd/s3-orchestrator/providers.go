// -------------------------------------------------------------------------------
// DI Providers - Service Construction Functions for samber/do
//
// Author: Alex Freidah
//
// Lazy provider functions for the dependency injection container. Each function
// resolves its own dependencies via do.Invoke and returns a fully constructed
// service. Optional components (encryption, cache, Redis, notifications) are
// only registered when their config section is enabled, so do.Invoke returns
// an error for disabled services — callers use the error to detect absence.
//
// Internal packages never import samber/do. Constructors keep explicit
// parameters. Only this file and main.go use the injector.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// INFRASTRUCTURE PROVIDERS
// -------------------------------------------------------------------------

// ProvideStore connects to PostgreSQL, runs migrations, and syncs quota limits.
func ProvideStore(i do.Injector) (*store.Store, error) {
	cfg := do.MustInvoke[*config.Config](i)
	ctx := context.Background()

	db, err := store.NewStore(ctx, &cfg.Database)
	if err != nil {
		return nil, err
	}

	if err := db.RunMigrations(ctx); err != nil {
		return nil, err
	}
	if err := db.VerifySchemaVersion(ctx); err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "Database migrations applied", "schema_version", store.ExpectedSchemaVersion)

	if err := db.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		return nil, err
	}

	return db, nil
}

// ProvideCBStore wraps the raw store with circuit breaker protection.
func ProvideCBStore(i do.Injector) (*store.CircuitBreakerStore, error) {
	cfg := do.MustInvoke[*config.Config](i)
	db := do.MustInvoke[*store.Store](i)
	return store.NewCircuitBreakerStore(db, cfg.CircuitBreaker), nil
}

// -------------------------------------------------------------------------
// BACKEND PROVIDERS
// -------------------------------------------------------------------------

// backendsResult groups the outputs of backend initialization.
type backendsResult struct {
	Backends    map[string]backend.ObjectBackend
	Order       []string
	UsageLimits map[string]store.UsageLimits
}

// ProvideBackends initializes all configured storage backends with optional
// per-backend circuit breakers.
func ProvideBackends(i do.Injector) (*backendsResult, error) {
	cfg := do.MustInvoke[*config.Config](i)

	backends := make(map[string]backend.ObjectBackend, len(cfg.Backends))
	order := make([]string, 0, len(cfg.Backends))
	limits := make(map[string]store.UsageLimits, len(cfg.Backends))

	for idx := range cfg.Backends {
		bcfg := &cfg.Backends[idx]
		s3be, err := backend.NewS3Backend(bcfg)
		if err != nil {
			return nil, err
		}
		var be backend.ObjectBackend = s3be
		if cfg.BackendCircuitBreaker.Enabled {
			be = backend.NewCircuitBreakerBackend(s3be, bcfg.Name,
				cfg.BackendCircuitBreaker.FailureThreshold,
				cfg.BackendCircuitBreaker.OpenTimeout)
		}
		backends[bcfg.Name] = be
		order = append(order, bcfg.Name)
		limits[bcfg.Name] = store.UsageLimits{
			APIRequestLimit:  bcfg.APIRequestLimit,
			EgressByteLimit:  bcfg.EgressByteLimit,
			IngressByteLimit: bcfg.IngressByteLimit,
		}
		slog.InfoContext(context.Background(), "Backend initialized",
			"backend", bcfg.Name,
			"endpoint", bcfg.Endpoint,
			"bucket", bcfg.Bucket,
		)
	}

	return &backendsResult{Backends: backends, Order: order, UsageLimits: limits}, nil
}

// -------------------------------------------------------------------------
// OPTIONAL COMPONENT PROVIDERS
// -------------------------------------------------------------------------

// ProvideEncryptor creates the envelope encryption engine.
func ProvideEncryptor(i do.Injector) (*encryption.Encryptor, error) {
	cfg := do.MustInvoke[*config.Config](i)
	provider, err := encryption.NewKeyProviderFromConfig(&cfg.Encryption)
	if err != nil {
		return nil, err
	}
	enc, err := encryption.NewEncryptor(provider, cfg.Encryption.ChunkSize)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "Server-side encryption enabled",
		"chunk_size", cfg.Encryption.ChunkSize,
		"key_id", provider.KeyID(),
	)
	return enc, nil
}

// ProvideEncryptionProvider creates the key provider for admin key rotation
// operations. Only registered when encryption is enabled.
func ProvideEncryptionProvider(i do.Injector) (encryption.KeyProvider, error) {
	cfg := do.MustInvoke[*config.Config](i)
	return encryption.NewKeyProviderFromConfig(&cfg.Encryption)
}

// ProvideRedisCounterBackend creates the shared Redis counter backend.
func ProvideRedisCounterBackend(i do.Injector) (*counter.RedisCounterBackend, error) {
	cfg := do.MustInvoke[*config.Config](i)
	br := do.MustInvoke[*backendsResult](i)

	redisOpts := &redis.Options{
		Addr:     cfg.Redis.Address,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	}
	if cfg.Redis.TLS {
		redisOpts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	redisClient := redis.NewClient(redisOpts)
	rb, err := counter.NewRedisCounterBackend(redisClient, cfg.Redis, br.Order)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "Redis shared counters enabled", "address", cfg.Redis.Address)
	return rb, nil
}

// ProvideObjectCache creates the in-memory LRU object data cache.
func ProvideObjectCache(i do.Injector) (objcache.ObjectCache, error) {
	cfg := do.MustInvoke[*config.Config](i)
	mc, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       cfg.Cache.MaxSizeBytes,
		MaxObjectSize: cfg.Cache.MaxObjectSizeBytes,
		TTL:           cfg.Cache.TTL,
	})
	if err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "Object data cache enabled",
		"max_size", cfg.Cache.MaxSize,
		"max_object_size", cfg.Cache.MaxObjectSize,
		"ttl", cfg.Cache.TTL,
	)
	return mc, nil
}

// -------------------------------------------------------------------------
// MANAGER PROVIDER
// -------------------------------------------------------------------------

// ProvideBackendManager creates the central orchestration manager.
func ProvideBackendManager(i do.Injector) (*proxy.BackendManager, error) {
	cfg := do.MustInvoke[*config.Config](i)
	cbStore := do.MustInvoke[*store.CircuitBreakerStore](i)
	br := do.MustInvoke[*backendsResult](i)

	// Optional: encryption
	var enc *encryption.Encryptor
	if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
		enc = e
	}

	// Optional: Redis counter backend
	var cb counter.CounterBackend
	if rb, err := do.Invoke[*counter.RedisCounterBackend](i); err == nil {
		cb = rb
	}

	// Optional: object cache
	var dataCache objcache.ObjectCache
	if c, err := do.Invoke[objcache.ObjectCache](i); err == nil {
		dataCache = c
	}

	// Admission semaphore
	var admissionSem chan struct{}
	if cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0 {
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentWrites)
	} else if cfg.Server.MaxConcurrentRequests > 0 {
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentRequests)
	}

	return proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:           br.Backends,
		Store:              cbStore,
		Order:              br.Order,
		CacheTTL:           cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:     cfg.Server.BackendTimeout,
		UsageLimits:        br.UsageLimits,
		RoutingStrategy:    cfg.RoutingStrategy,
		ParallelBroadcast:  cfg.CircuitBreaker.ParallelBroadcast,
		Encryptor:          enc,
		ObjectCache:        dataCache,
		CounterBackend:     cb,
		CleanupConcurrency: cfg.CleanupQueue.Concurrency,
		AdmissionSem:       admissionSem,
	}), nil
}

// -------------------------------------------------------------------------
// BACKGROUND SERVICE PROVIDERS
// -------------------------------------------------------------------------

// ProvideLifecycleManager creates and registers all background services.
func ProvideLifecycleManager(i do.Injector) (*lifecycle.Manager, error) {
	cfg := do.MustInvoke[*config.Config](i)
	manager := do.MustInvoke[*proxy.BackendManager](i)
	cbStore := do.MustInvoke[*store.CircuitBreakerStore](i)
	mode := do.MustInvoke[string](i)

	sm := lifecycle.NewManager()
	sm.Register("usage-flush", &usageFlushService{manager: manager, locker: cbStore})

	if mode == "worker" || mode == "all" {
		sm.Register("multipart-cleanup", newMultipartCleanupService(manager, cbStore, cfg.CleanupQueue.MultipartStaleTimeout))
		sm.Register("cleanup-queue", newCleanupQueueService(manager, cbStore))
		sm.Register("rebalancer", newRebalancerService(manager, cbStore))
		sm.Register("replicator", newReplicatorService(manager, cbStore))
		sm.Register("over-replication", newOverReplicationService(manager, cbStore))
		sm.Register("lifecycle", newLifecycleService(manager, cbStore))
		sm.Register("scrubber", newScrubberService(manager, cbStore))

		if cfg.Reconcile.Enabled {
			bktNames := make([]string, len(cfg.Buckets))
			for idx, b := range cfg.Buckets {
				bktNames[idx] = b.Name
			}
			reconciler := worker.NewReconciler(manager, bktNames)
			sm.Register("reconcile", newReconcileService(reconciler, cbStore, cfg.Reconcile.Interval))
		}

		// Optional: notification delivery worker
		if notifier, err := do.Invoke[*notify.Notifier](i); err == nil {
			sm.Register("notifications", notifier)
		}
	}

	return sm, nil
}

// -------------------------------------------------------------------------
// HTTP LAYER PROVIDERS
// -------------------------------------------------------------------------

// ProvideBucketAuth creates the credential-to-bucket registry.
func ProvideBucketAuth(i do.Injector) (*auth.BucketRegistry, error) {
	cfg := do.MustInvoke[*config.Config](i)
	return auth.NewBucketRegistry(cfg.Buckets), nil
}

// ProvideS3Server creates the S3-compatible HTTP handler.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
	cfg := do.MustInvoke[*config.Config](i)
	manager := do.MustInvoke[*proxy.BackendManager](i)
	bucketAuth := do.MustInvoke[*auth.BucketRegistry](i)

	srv := s3api.NewServer(manager, cfg.Server.MaxObjectSize)
	srv.SetBucketAuth(bucketAuth)
	return srv, nil
}

// ProvideRateLimiter creates the per-IP rate limiter.
func ProvideRateLimiter(i do.Injector) (*s3api.RateLimiter, error) {
	cfg := do.MustInvoke[*config.Config](i)
	rl := s3api.NewRateLimiter(cfg.RateLimit)
	slog.InfoContext(context.Background(), "Rate limiting enabled",
		"requests_per_sec", cfg.RateLimit.RequestsPerSec,
		"burst", cfg.RateLimit.Burst,
	)
	return rl, nil
}

// ProvideLoginThrottle creates the per-IP login attempt throttle.
func ProvideLoginThrottle(i do.Injector) (*httputil.LoginThrottle, error) {
	return httputil.NewLoginThrottle(5, 5*time.Minute), nil
}

// ProvideUIHandler creates the web dashboard handler.
func ProvideUIHandler(i do.Injector) (*ui.Handler, error) {
	cfg := do.MustInvoke[*config.Config](i)
	manager := do.MustInvoke[*proxy.BackendManager](i)
	cbStore := do.MustInvoke[*store.CircuitBreakerStore](i)
	logBuffer := do.MustInvoke[*telemetry.LogBuffer](i)
	loginThrottle := do.MustInvoke[*httputil.LoginThrottle](i)

	return ui.New(manager, cbStore.IsHealthy, cfg, logBuffer, loginThrottle), nil
}

// ProvideAdminHandler creates the admin API handler.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	cfg := do.MustInvoke[*config.Config](i)
	manager := do.MustInvoke[*proxy.BackendManager](i)
	cbStore := do.MustInvoke[*store.CircuitBreakerStore](i)
	db := do.MustInvoke[*store.Store](i)
	logLevel := do.MustInvoke[*slog.LevelVar](i)

	var enc *encryption.Encryptor
	if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
		enc = e
	}

	adminToken := cfg.UI.AdminToken
	if adminToken == "" {
		adminToken = cfg.UI.AdminKey
	}

	return admin.New(manager, cbStore, db, enc, adminToken, logLevel), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	cfg := do.MustInvoke[*config.Config](i)
	db := do.MustInvoke[*store.Store](i)
	return notify.NewNotifier(&cfg.Notifications, db), nil
}

// ProvideLogBuffer creates the in-memory log ring buffer for the dashboard.
func ProvideLogBuffer(_ do.Injector) (*telemetry.LogBuffer, error) {
	return telemetry.NewLogBuffer(), nil
}

// -------------------------------------------------------------------------
// AUDIT WIRING
// -------------------------------------------------------------------------

// wireAuditMetrics connects the audit event counter to Prometheus.
func wireAuditMetrics() {
	audit.OnEvent = func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	}
}
