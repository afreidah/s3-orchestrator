// -------------------------------------------------------------------------------
// Dependency Injection — Single Wiring Point for samber/do
//
// Author: Alex Freidah
//
// NewInjector creates the DI container and registers every provider the
// service needs. Providers are lazy — nothing is constructed until the
// corresponding do.Invoke call. Optional components (encryption, cache,
// Redis, notifications) register only when enabled in config; do.Invoke
// returns an error for disabled services, which callers use to detect
// absence.
//
// Narrow-role store providers are registered one per role (ObjectStore,
// QuotaStore, CleanupStore, ...) so consumers can ask only for the slice
// they actually use. Each narrow provider wraps the concrete *store.Store
// with the per-role CB decorator. No consumer ever sees a composed "god
// interface" — that type no longer exists.
//
// Non-DI packages (internal/*, internal/transport/*) never import samber/do.
// Constructors keep explicit parameters; only this package and cmd/ touch
// the injector.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
	sqlitestore "github.com/afreidah/s3-orchestrator/internal/store/sqlite"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// INJECTOR CONSTRUCTION
// -------------------------------------------------------------------------

// NewInjector creates and configures the DI container. Required providers
// are always registered. Optional providers register only when their config
// section is enabled — do.Invoke returns an error for disabled services,
// which callers use to detect absence.
func NewInjector(cfg *config.Config, mode string, logLevel *slog.LevelVar, logBuffer *telemetry.LogBuffer) do.Injector {
	inj := do.New()

	// --- Value providers (already-constructed) ---
	do.ProvideValue(inj, cfg)
	do.ProvideNamedValue(inj, "mode", mode)
	do.ProvideValue(inj, logLevel)
	do.ProvideValue(inj, logBuffer)

	// --- Required infrastructure ---
	do.Provide(inj, ProvideConcreteStore)
	do.Provide(inj, ProvideAdminStore)
	do.Provide(inj, ProvideDatabaseBreaker)

	// Narrow per-role store providers — each wraps ProvideConcreteStore's
	// value with its per-role CB decorator.
	do.Provide(inj, ProvideObjectStore)
	do.Provide(inj, ProvideQuotaStore)
	do.Provide(inj, ProvideMultipartStore)
	do.Provide(inj, ProvideReplicationStore)
	do.Provide(inj, ProvideCleanupStore)
	do.Provide(inj, ProvideIntegrityStore)
	do.Provide(inj, ProvideLifecycleStore)
	do.Provide(inj, ProvideBackendLifecycleStore)
	do.Provide(inj, ProvideDashboardStore)
	do.Provide(inj, ProvideUsageFlusher)
	do.Provide(inj, ProvideAdvisoryLocker)
	do.Provide(inj, ProvideMetricsDeps)

	do.Provide(inj, ProvideBackends)
	do.Provide(inj, ProvideBackendManager)
	do.Provide(inj, ProvideBucketAuth)
	do.Provide(inj, ProvideS3Server)
	do.Provide(inj, ProvideLifecycleManager)

	// --- Optional features (registered only when enabled) ---
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

	return inj
}

// -------------------------------------------------------------------------
// STORE PROVIDERS
// -------------------------------------------------------------------------

// concreteStoreBundle groups the concrete driver handle (narrow-role
// carrier) and the admin-only interface. Both PostgreSQL *store.Store and
// SQLite *sqlite.Store satisfy every narrow role plus AdminStore.
type concreteStoreBundle struct {
	concrete concreteStore
	admin    store.AdminStore
}

// concreteStore collects the role interfaces satisfied by the driver-level
// store without introducing a user-facing composed type. Declared
// unexported and scoped to this package — callers outside di never see it.
type concreteStore interface {
	store.ObjectStore
	store.QuotaStore
	store.MultipartStore
	store.ReplicationStore
	store.CleanupStore
	store.IntegrityStore
	store.LifecycleStore
	store.BackendLifecycleStore
	store.DashboardStore
	store.UsageFlusher
	store.AdvisoryLocker
	metricsDeps
}

// metricsDeps names the five methods proxy.MetricsDeps requires. Declared
// here (not exported) because it only exists so concreteStore satisfies the
// proxy-owned MetricsDeps contract structurally.
type metricsDeps interface {
	GetQuotaStats(ctx context.Context) (map[string]store.QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]store.UsageStat, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]store.ObjectLocation, error)
}

// ProvideConcreteStore creates the concrete driver store for the configured
// driver, runs migrations, and syncs quota limits. Returned as an unexported
// composite; no call site outside this package references it directly.
func ProvideConcreteStore(i do.Injector) (*concreteStoreBundle, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	cs, adminDB, err := openStore(ctx, &cfg.Database)
	if err != nil {
		return nil, err
	}
	if err := adminDB.RunMigrations(ctx); err != nil {
		return nil, err
	}
	if err := adminDB.VerifySchemaVersion(ctx); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "database migrations applied", "driver", cfg.Database.Driver)

	if err := adminDB.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		return nil, err
	}

	return &concreteStoreBundle{concrete: cs, admin: adminDB}, nil
}

// ProvideAdminStore extracts the AdminStore from the concrete bundle.
func ProvideAdminStore(i do.Injector) (store.AdminStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	return b.admin, nil
}

// ProvideDatabaseBreaker constructs the shared *breaker.CircuitBreaker that
// every per-role CB wrapper forwards calls through.
func ProvideDatabaseBreaker(i do.Injector) (*breaker.CircuitBreaker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return store.NewDatabaseBreaker(cfg.CircuitBreaker), nil
}

// openStore dispatches store construction to the configured driver.
func openStore(ctx context.Context, dbCfg *config.DatabaseConfig) (concreteStore, store.AdminStore, error) {
	switch dbCfg.Driver {
	case "postgres":
		s, err := store.NewStore(ctx, dbCfg)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	case "sqlite":
		s, err := sqlitestore.NewStore(ctx, dbCfg)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	default:
		return nil, nil, fmt.Errorf("unsupported database driver: %q", dbCfg.Driver)
	}
}

// -------------------------------------------------------------------------
// NARROW PER-ROLE STORE PROVIDERS
//
// Each returns the concrete driver wrapped by the appropriate CB decorator,
// typed only as the narrow role interface. Consumers do.Invoke the role
// they need.
// -------------------------------------------------------------------------

// ProvideObjectStore registers a CB-protected ObjectStore view.
func ProvideObjectStore(i do.Injector) (store.ObjectStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBObjectStore(b.concrete, cb), nil
}

// ProvideQuotaStore registers a CB-protected QuotaStore view.
func ProvideQuotaStore(i do.Injector) (store.QuotaStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBQuotaStore(b.concrete, cb), nil
}

// ProvideMultipartStore registers a CB-protected MultipartStore view.
func ProvideMultipartStore(i do.Injector) (store.MultipartStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBMultipartStore(b.concrete, cb), nil
}

// ProvideReplicationStore registers a CB-protected ReplicationStore view.
func ProvideReplicationStore(i do.Injector) (store.ReplicationStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBReplicationStore(b.concrete, cb), nil
}

// ProvideCleanupStore registers a CB-protected CleanupStore view.
func ProvideCleanupStore(i do.Injector) (store.CleanupStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBCleanupStore(b.concrete, cb), nil
}

// ProvideIntegrityStore registers a CB-protected IntegrityStore view.
func ProvideIntegrityStore(i do.Injector) (store.IntegrityStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBIntegrityStore(b.concrete, cb), nil
}

// ProvideLifecycleStore registers a CB-protected LifecycleStore view.
func ProvideLifecycleStore(i do.Injector) (store.LifecycleStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBLifecycleStore(b.concrete, cb), nil
}

// ProvideBackendLifecycleStore registers a CB-protected BackendLifecycleStore view.
func ProvideBackendLifecycleStore(i do.Injector) (store.BackendLifecycleStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBBackendLifecycleStore(b.concrete, cb), nil
}

// ProvideDashboardStore registers a CB-protected DashboardStore view.
func ProvideDashboardStore(i do.Injector) (store.DashboardStore, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBDashboardStore(b.concrete, cb), nil
}

// ProvideUsageFlusher registers a CB-protected UsageFlusher view.
func ProvideUsageFlusher(i do.Injector) (store.UsageFlusher, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBUsageFlusher(b.concrete, cb), nil
}

// ProvideAdvisoryLocker registers a pass-through AdvisoryLocker — advisory
// locks bypass the breaker (see internal/store/cb_lock.go).
func ProvideAdvisoryLocker(i do.Injector) (store.AdvisoryLocker, error) {
	b, err := do.Invoke[*concreteStoreBundle](i)
	if err != nil {
		return nil, err
	}
	return store.NewAdvisoryLocker(b.concrete), nil
}

// metricsDepsAdapter composes the narrow roles MetricsCollector queries.
// Struct embedding promotes each method to the top-level type, so the
// adapter structurally satisfies proxy.MetricsDeps without inventing a
// composed interface.
type metricsDepsAdapter struct {
	store.DashboardStore   // GetQuotaStats, GetObjectCounts, GetActiveMultipartCounts, GetUsageForPeriod
	store.ReplicationStore // GetUnderReplicatedObjects (among others)
}

// ProvideMetricsDeps builds the adapter MetricsCollector uses to refresh
// Prometheus gauges.
func ProvideMetricsDeps(i do.Injector) (proxy.MetricsDeps, error) {
	dash, err := do.Invoke[store.DashboardStore](i)
	if err != nil {
		return nil, err
	}
	repl, err := do.Invoke[store.ReplicationStore](i)
	if err != nil {
		return nil, err
	}
	return &metricsDepsAdapter{DashboardStore: dash, ReplicationStore: repl}, nil
}

// -------------------------------------------------------------------------
// BACKEND PROVIDERS
// -------------------------------------------------------------------------

// BackendsResult groups the outputs of backend initialization so multiple
// providers can resolve it without re-running construction.
type BackendsResult struct {
	Backends       map[string]backend.ObjectBackend
	Order          []string
	UsageLimits    map[string]store.UsageLimits
	MaxObjectSizes map[string]int64
}

// ProvideBackends initializes all configured storage backends, wrapping
// each with a per-backend circuit breaker when enabled.
func ProvideBackends(i do.Injector) (*BackendsResult, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}

	backends := make(map[string]backend.ObjectBackend, len(cfg.Backends))
	order := make([]string, 0, len(cfg.Backends))
	limits := make(map[string]store.UsageLimits, len(cfg.Backends))
	maxSizes := make(map[string]int64, len(cfg.Backends))

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
		if bcfg.MaxObjectSize > 0 {
			maxSizes[bcfg.Name] = bcfg.MaxObjectSize
		}
		slog.InfoContext(context.Background(), "backend initialized",
			"backend", bcfg.Name,
			"endpoint", bcfg.Endpoint,
			"bucket", bcfg.Bucket,
		)
	}

	return &BackendsResult{Backends: backends, Order: order, UsageLimits: limits, MaxObjectSizes: maxSizes}, nil
}

// -------------------------------------------------------------------------
// OPTIONAL COMPONENT PROVIDERS
// -------------------------------------------------------------------------

// ProvideEncryptor creates the envelope encryption engine.
func ProvideEncryptor(i do.Injector) (*encryption.Encryptor, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	provider, err := encryption.NewKeyProviderFromConfig(&cfg.Encryption)
	if err != nil {
		return nil, err
	}
	enc, err := encryption.NewEncryptor(provider, cfg.Encryption.ChunkSize)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "server-side encryption enabled",
		"chunk_size", cfg.Encryption.ChunkSize,
		"key_id", provider.KeyID(),
	)
	return enc, nil
}

// ProvideEncryptionProvider creates the key provider for admin key rotation
// operations. Only registered when encryption is enabled.
func ProvideEncryptionProvider(i do.Injector) (encryption.KeyProvider, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return encryption.NewKeyProviderFromConfig(&cfg.Encryption)
}

// ProvideRedisCounterBackend creates the shared Redis counter backend.
func ProvideRedisCounterBackend(i do.Injector) (*counter.RedisCounterBackend, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}

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
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mc, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       cfg.Cache.MaxSizeBytes,
		MaxObjectSize: cfg.Cache.MaxObjectSizeBytes,
		TTL:           cfg.Cache.TTL,
	})
	if err != nil {
		return nil, err
	}
	slog.InfoContext(context.Background(), "object data cache enabled",
		"max_size", cfg.Cache.MaxSize,
		"max_object_size", cfg.Cache.MaxObjectSize,
		"ttl", cfg.Cache.TTL,
	)
	return mc, nil
}

// -------------------------------------------------------------------------
// MANAGER PROVIDER
// -------------------------------------------------------------------------

// ProvideBackendManager creates the central orchestration manager with the
// narrow per-role store interfaces supplied.
func ProvideBackendManager(i do.Injector) (*proxy.BackendManager, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}
	stores, err := resolveProxyStores(i)
	if err != nil {
		return nil, err
	}
	dash, err := do.Invoke[store.DashboardStore](i)
	if err != nil {
		return nil, err
	}
	metrics, err := do.Invoke[proxy.MetricsDeps](i)
	if err != nil {
		return nil, err
	}

	// Encryption: required when enabled, fatal on failure.
	var enc *encryption.Encryptor
	if cfg.Encryption.Enabled {
		e, err := do.Invoke[*encryption.Encryptor](i)
		if err != nil {
			return nil, fmt.Errorf("encryption enabled but encryptor failed to initialize: %w", err)
		}
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
	switch {
	case cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0:
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentWrites)
	case cfg.Server.MaxConcurrentRequests > 0:
		admissionSem = make(chan struct{}, cfg.Server.MaxConcurrentRequests)
	}

	return proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:           br.Backends,
		Stores:             stores,
		Metrics:            metrics,
		Dashboard:          dash,
		Order:              br.Order,
		CacheTTL:           cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:     cfg.Server.BackendTimeout,
		UsageLimits:        br.UsageLimits,
		RoutingStrategy:    cfg.RoutingStrategy,
		ParallelBroadcast:  cfg.CircuitBreaker.ParallelBroadcast,
		Encryptor:          enc,
		ObjectCache:        dataCache,
		CounterBackend:     cb,
		MaxObjectSizes:     br.MaxObjectSizes,
		CleanupConcurrency: cfg.CleanupQueue.Concurrency,
		AdmissionSem:       admissionSem,
	}), nil
}

// resolveProxyStores assembles the proxy.Stores bag by invoking each narrow
// role provider. Defined here so ProvideBackendManager stays readable.
func resolveProxyStores(i do.Injector) (proxy.Stores, error) {
	obj, err := do.Invoke[store.ObjectStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	q, err := do.Invoke[store.QuotaStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	mp, err := do.Invoke[store.MultipartStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	rep, err := do.Invoke[store.ReplicationStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	cu, err := do.Invoke[store.CleanupStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	ig, err := do.Invoke[store.IntegrityStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	lc, err := do.Invoke[store.LifecycleStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	blc, err := do.Invoke[store.BackendLifecycleStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	dash, err := do.Invoke[store.DashboardStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	uf, err := do.Invoke[store.UsageFlusher](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	lock, err := do.Invoke[store.AdvisoryLocker](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	return proxy.Stores{
		Object:           obj,
		Quota:            q,
		Multipart:        mp,
		Replication:      rep,
		Cleanup:          cu,
		Integrity:        ig,
		Lifecycle:        lc,
		BackendLifecycle: blc,
		Dashboard:        dash,
		Usage:            uf,
		Lock:             lock,
	}, nil
}

// -------------------------------------------------------------------------
// BACKGROUND SERVICE PROVIDERS
// -------------------------------------------------------------------------

// ProvideLifecycleManager creates and registers all background services.
func ProvideLifecycleManager(i do.Injector) (*lifecycle.Manager, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	locker, err := do.Invoke[store.AdvisoryLocker](i)
	if err != nil {
		return nil, err
	}
	mode, err := do.InvokeNamed[string](i, "mode")
	if err != nil {
		return nil, err
	}

	sm := lifecycle.NewManager()
	sm.Register("usage-flush", NewUsageFlushService(manager, locker))
	sm.Register("cb-watchdog", NewCircuitBreakerWatchdog(manager, cb))

	if mode == "worker" || mode == "all" {
		sm.Register("multipart-cleanup", NewMultipartCleanupService(manager, locker, cfg.CleanupQueue.MultipartStaleTimeout))
		sm.Register("cleanup-queue", NewCleanupQueueService(manager, locker))
		sm.Register("rebalancer", NewRebalancerService(manager, locker))
		sm.Register("replicator", NewReplicatorService(manager, locker))
		sm.Register("over-replication", NewOverReplicationService(manager, locker))
		sm.Register("lifecycle", NewLifecycleService(manager, locker))
		sm.Register("scrubber", NewScrubberService(manager, locker))

		bktNames := make([]string, len(cfg.Buckets))
		for idx, b := range cfg.Buckets {
			bktNames[idx] = b.Name
		}
		reconciler := worker.NewReconciler(manager, bktNames)
		do.ProvideValue(i, reconciler)

		if cfg.Reconcile.Enabled {
			sm.Register("reconcile", NewReconcileService(reconciler, locker, cfg.Reconcile.Interval))
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
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return auth.NewBucketRegistry(cfg.Buckets), nil
}

// ProvideS3Server creates the S3-compatible HTTP handler.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	bucketAuth, err := do.Invoke[*auth.BucketRegistry](i)
	if err != nil {
		return nil, err
	}
	srv := s3api.NewServer(manager, cfg.Server.MaxObjectSize)
	srv.SetBucketAuth(bucketAuth)
	return srv, nil
}

// ProvideRateLimiter creates the per-IP rate limiter.
func ProvideRateLimiter(i do.Injector) (*s3api.RateLimiter, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	rl := s3api.NewRateLimiter(cfg.RateLimit)
	slog.InfoContext(context.Background(), "rate limiting enabled",
		"requests_per_sec", cfg.RateLimit.RequestsPerSec,
		"burst", cfg.RateLimit.Burst,
	)
	return rl, nil
}

// ProvideLoginThrottle creates the per-IP login attempt throttle.
func ProvideLoginThrottle(_ do.Injector) (*httputil.LoginThrottle, error) {
	return httputil.NewLoginThrottle(5, 5*time.Minute), nil
}

// ProvideUIHandler creates the web dashboard handler.
func ProvideUIHandler(i do.Injector) (*ui.Handler, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	logBuffer, err := do.Invoke[*telemetry.LogBuffer](i)
	if err != nil {
		return nil, err
	}
	loginThrottle, err := do.Invoke[*httputil.LoginThrottle](i)
	if err != nil {
		return nil, err
	}
	return ui.New(manager, cb.IsHealthy, cfg, logBuffer, loginThrottle), nil
}

// ProvideAdminHandler creates the admin API handler.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	manager, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	adminDB, err := do.Invoke[store.AdminStore](i)
	if err != nil {
		return nil, err
	}
	logLevel, err := do.Invoke[*slog.LevelVar](i)
	if err != nil {
		return nil, err
	}

	var enc *encryption.Encryptor
	if cfg.Encryption.Enabled {
		e, err := do.Invoke[*encryption.Encryptor](i)
		if err != nil {
			return nil, fmt.Errorf("encryption enabled but encryptor failed to initialize: %w", err)
		}
		enc = e
	}

	var reconciler *worker.Reconciler
	if r, err := do.Invoke[*worker.Reconciler](i); err == nil {
		reconciler = r
	}

	adminToken := cfg.UI.AdminToken
	if adminToken == "" {
		adminToken = cfg.UI.AdminKey
	}

	objects, err := do.Invoke[store.ObjectStore](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[store.CleanupStore](i)
	if err != nil {
		return nil, err
	}

	return admin.New(manager, cb, adminDB, enc, reconciler, adminToken, logLevel, objects, cleanup), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	adminDB, err := do.Invoke[store.AdminStore](i)
	if err != nil {
		return nil, err
	}
	return notify.NewNotifier(&cfg.Notifications, adminDB), nil
}

// ProvideLogBuffer creates the in-memory log ring buffer for the dashboard.
func ProvideLogBuffer(_ do.Injector) (*telemetry.LogBuffer, error) {
	return telemetry.NewLogBuffer(), nil
}

// -------------------------------------------------------------------------
// AUDIT WIRING
// -------------------------------------------------------------------------

// WireAuditMetrics connects the audit event counter to Prometheus. Called
// from the main binary during startup, outside the injector.
func WireAuditMetrics() {
	audit.SetOnEvent(func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	})
}