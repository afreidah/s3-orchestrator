// -------------------------------------------------------------------------------
// Dependency Injection  -  Single Wiring Point for samber/do
//
// Author: Alex Freidah
//
// NewInjector creates the DI container and registers every provider the
// service needs. Providers are lazy  -  nothing is constructed until the
// corresponding do.Invoke call. Optional components (encryption, cache,
// Redis, notifications) register only when enabled in config; do.Invoke
// returns an error for disabled services, which callers use to detect
// absence.
//
// Narrow-role store providers are registered one per role (ObjectStore,
// QuotaStore, CleanupStore, ...) so consumers can ask only for the slice
// they actually use. Each narrow provider wraps the concrete *store.Store
// with the per-role CB decorator. No consumer ever sees a composed "god
// interface"  -  that type no longer exists.
//
// Non-DI packages (internal/*, internal/transport/*) never import samber/do.
// Constructors keep explicit parameters; only this package and cmd/ touch
// the injector.
// -------------------------------------------------------------------------------

// Package di is the single wiring point for the orchestrator. It uses
// samber/do/v2 to register every store role, backend, worker, and
// transport handler as a lazy provider; consumers resolve their
// dependencies through the injector at the moment they need them.
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
	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/notify"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
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
// section is enabled  -  do.Invoke returns an error for disabled services,
// which callers use to detect absence.
func NewInjector(cfg *config.Config, mode string, logLevel *slog.LevelVar, logBuffer *telemetry.LogBuffer) do.Injector {
	inj := do.New()

	// --- Value providers (already-constructed) ---
	do.ProvideValue(inj, cfg)
	do.ProvideNamedValue(inj, "mode", mode)
	do.ProvideValue(inj, logLevel)
	do.ProvideValue(inj, logBuffer)

	// --- Required infrastructure ---
	do.Provide(inj, ProvideMetadataStore)
	do.Provide(inj, ProvideLifecycleAdmin)
	do.Provide(inj, ProvideEncryptionAdmin)
	do.Provide(inj, ProvideNotificationOutbox)
	do.Provide(inj, ProvideDatabaseBreaker)
	do.Provide(inj, ProvideInstanceID)
	do.Provide(inj, ProvideMetricsDeps)

	do.Provide(inj, ProvideBackends)
	do.Provide(inj, ProvideBreakerRegistry)
	do.Provide(inj, ProvideBackendManager)

	// Worker providers  -  each takes BackendManager (worker.Ops) plus the
	// per-worker store role from above. drain.Manager wires itself onto
	// BackendManager via WireDrain so backendCore's eligibility filters
	// see drain state.
	do.Provide(inj, ProvideRebalancer)
	do.Provide(inj, ProvideReplicator)
	do.Provide(inj, ProvideOverReplicationCleaner)
	do.Provide(inj, ProvideCleanupWorker)
	do.Provide(inj, ProvidePendingReaper)
	do.Provide(inj, ProvideScrubber)
	do.Provide(inj, ProvideDrainManager)

	do.Provide(inj, ProvideBucketAuth)
	do.Provide(inj, ProvideS3Server)
	do.Provide(inj, ProvideLifecycleManager)

	// --- Worker-mode services (registered only in worker/all modes) ---
	if mode == "worker" || mode == "all" {
		do.Provide(inj, ProvideReconciler)
	}

	// --- Optional features (registered only when enabled) ---
	if cfg.Encryption.Enabled {
		do.Provide(inj, ProvideEncryptor)
		do.Provide(inj, ProvideEncryptionProvider)
		do.Provide(inj, ProvideMultipartBackfill)
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

// ProvideMetadataStore opens the configured driver, runs migrations,
// syncs quota limits, and returns the wide core.MetadataStore every
// consumer depends on. CB protection lives inside each driver's DBTX/DB
// chokepoint, so this provider does no wrapping of its own.
func ProvideMetadataStore(i do.Injector) (core.MetadataStore, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	cs, err := openStore(ctx, &cfg.Database, cb)
	if err != nil {
		return nil, err
	}
	if err := cs.RunMigrations(ctx); err != nil {
		return nil, err
	}
	if err := cs.VerifySchemaVersion(ctx); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "database migrations applied", "driver", cfg.Database.Driver)

	if err := cs.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		return nil, err
	}
	return cs, nil
}

// ProvideLifecycleAdmin aliases the wide MetadataStore as its LifecycleAdmin
// role for boot/shutdown migrations, schema checks, and Close.
func ProvideLifecycleAdmin(i do.Injector) (core.LifecycleAdmin, error) {
	return do.Invoke[core.MetadataStore](i)
}

// ProvideEncryptionAdmin aliases the wide MetadataStore as its EncryptionAdmin
// role for the admin HTTP handler's key rotation and encrypt/decrypt batch ops.
func ProvideEncryptionAdmin(i do.Injector) (core.EncryptionAdmin, error) {
	return do.Invoke[core.MetadataStore](i)
}

// ProvideNotificationOutbox aliases the wide MetadataStore as its
// NotificationOutbox role for the notifier worker.
func ProvideNotificationOutbox(i do.Injector) (core.NotificationOutbox, error) {
	return do.Invoke[core.MetadataStore](i)
}

// ProvideDatabaseBreaker constructs the shared *breaker.CircuitBreaker every
// driver-level SQL statement forwards calls through.
func ProvideDatabaseBreaker(i do.Injector) (*breaker.CircuitBreaker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return store.NewDatabaseBreaker(cfg.CircuitBreaker), nil
}

// openStore dispatches store construction to the configured driver. The
// shared *breaker.CircuitBreaker is threaded through so every SQL
// statement (pool-bound or tx-bound) gets PreCheck/PostCheck protection
// at the driver chokepoint.
func openStore(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker) (core.MetadataStore, error) {
	switch dbCfg.Driver {
	case "postgres":
		return postgres.NewStore(ctx, dbCfg, cb)
	case "sqlite":
		return sqlitestore.NewStore(ctx, dbCfg, cb)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", dbCfg.Driver)
	}
}

// ProvideInstanceID resolves a stable per-process identifier used as the
// claimed_by stamp on cleanup_queue rows. The identifier is generated once
// at first invoke and reused everywhere the value is needed.
func ProvideInstanceID(_ do.Injector) (instanceid.ID, error) {
	return instanceid.New()
}

// ProvideMetricsDeps aliases the wide MetadataStore as the metrics.Deps
// subset metrics.Collector uses to refresh Prometheus gauges.
func ProvideMetricsDeps(i do.Injector) (metrics.Deps, error) {
	return do.Invoke[core.MetadataStore](i)
}

// -------------------------------------------------------------------------
// WORKER PROVIDERS
//
// Each worker is its own DI service. Providers invoke *proxy.BackendManager
// (which satisfies worker.Ops / CleanupOps / ScrubberOps via promoted
// backendCore methods plus its own write-path helpers) and the wide
// core.MetadataStore, which already satisfies every per-worker store
// contract via implicit interface satisfaction.
// -------------------------------------------------------------------------

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	rb := worker.NewRebalancer(mgr, stores)
	mgr.Rebalancer = rb
	return rb, nil
}

// ProvideReplicator constructs the replication worker.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	rp := worker.NewReplicator(mgr, stores)
	mgr.Replicator = rp
	return rp, nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	or := worker.NewOverReplicationCleaner(mgr, stores)
	mgr.OverReplicationCleaner = or
	return or, nil
}

// ProvideCleanupWorker constructs the cleanup-queue worker.
func ProvideCleanupWorker(i do.Injector) (*worker.CleanupWorker, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	id, err := do.Invoke[instanceid.ID](i)
	if err != nil {
		return nil, err
	}
	cw := worker.NewCleanupWorker(mgr, cleanup, concurrency, id.String(), cfg.CleanupQueue.ClaimGracePeriod)
	mgr.CleanupWorker = cw
	return cw, nil
}

// ProvidePendingReaper constructs the pending-reaper worker. Returns nil
// when the pending pattern is disabled (matches the legacy NewBackendManager
// behavior of only attaching a reaper when stores.Pending is non-nil).
func ProvidePendingReaper(i do.Injector) (*worker.PendingReaper, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	if !cfg.WritePath.PendingPattern.IsEnabled() {
		return nil, nil //nolint:nilnil // intentional: nil signals feature off
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	pending, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, nil //nolint:nilnil // store provider returned nil, feature off
	}
	pr := worker.NewPendingReaper(mgr, pending, 0, cfg.WritePath.PendingPattern.MinAge, cfg.WritePath.PendingPattern.BatchSize)
	mgr.PendingReaper = pr
	return pr, nil
}

// ProvideScrubber constructs the integrity-verification worker.
func ProvideScrubber(i do.Injector) (*worker.Scrubber, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	integrity, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	var enc *encryption.Encryptor
	if cfg.Encryption.Enabled {
		if e, err := do.Invoke[*encryption.Encryptor](i); err == nil {
			enc = e
		}
	}
	sc := worker.NewScrubber(mgr, integrity, enc)
	mgr.Scrubber = sc
	return sc, nil
}

// ProvideReconciler constructs the bucket reconciler worker. Registered
// only in worker/all modes because reconciliation is a worker-side
// background task. Returns the reconciler so the lifecycle manager can
// register a service for it; the reconciler is also resolvable directly
// for the admin handler's inspection endpoints.
func ProvideReconciler(i do.Injector) (*worker.Reconciler, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	bktNames := make([]string, len(cfg.Buckets))
	for idx, b := range cfg.Buckets {
		bktNames[idx] = b.Name
	}
	return worker.NewReconciler(mgr, bktNames), nil
}

// ProvideMultipartBackfill constructs the legacy-multipart-DEK migration
// worker. Registered only when encryption is enabled because legacy rows
// can only exist when the proxy was previously running with encryption.
func ProvideMultipartBackfill(i do.Injector) (*worker.MultipartBackfill, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	enc, err := do.Invoke[*encryption.Encryptor](i)
	if err != nil {
		return nil, err
	}
	return worker.NewMultipartBackfill(stores, enc, mgr.GetBackend, worker.MultipartBackfillConfig{}), nil
}

// ProvideDrainManager constructs the drain manager. Depends on
// BackendManager (drain.Core seam), the cleanup worker (for the
// cleanup-queue flush before backend deletion), and the wide
// MetadataStore for the object/quota/lifecycle role surfaces.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[*worker.CleanupWorker](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	dm := drain.New(
		mgr,
		stores,
		stores,
		stores,
		mgr.MultipartManager.AbortMultipartUploadsOnBackend,
		cleanup.ProcessCleanupQueue,
	)
	mgr.WireDrain(dm)
	return dm, nil
}

// -------------------------------------------------------------------------
// BACKEND PROVIDERS
// -------------------------------------------------------------------------

// BackendsResult groups the outputs of backend initialization so multiple
// providers can resolve it without re-running construction.
type BackendsResult struct {
	Backends       map[string]backend.ObjectBackend
	Order          []string
	UsageLimits    map[string]core.UsageLimits
	MaxObjectSizes map[string]int64
	// Breakers is the per-backend circuit breakers produced when
	// BackendCircuitBreaker is enabled. Empty when CBs are disabled.
	// The watchdog registry consumes this so it never has to rediscover
	// breakers via type assertion at runtime.
	Breakers []breaker.StaleProbeResetter
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
	limits := make(map[string]core.UsageLimits, len(cfg.Backends))
	maxSizes := make(map[string]int64, len(cfg.Backends))
	breakers := make([]breaker.StaleProbeResetter, 0, len(cfg.Backends))

	for idx := range cfg.Backends {
		bcfg := &cfg.Backends[idx]
		s3be, err := backend.NewS3Backend(bcfg)
		if err != nil {
			return nil, err
		}
		var be backend.ObjectBackend = s3be
		if cfg.BackendCircuitBreaker.Enabled {
			cbBackend := backend.NewCircuitBreakerBackend(s3be, bcfg.Name,
				cfg.BackendCircuitBreaker.FailureThreshold,
				cfg.BackendCircuitBreaker.OpenTimeout)
			breakers = append(breakers, cbBackend)
			be = cbBackend
		}
		backends[bcfg.Name] = be
		order = append(order, bcfg.Name)
		limits[bcfg.Name] = core.UsageLimits{
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

	return &BackendsResult{
		Backends:       backends,
		Order:          order,
		UsageLimits:    limits,
		MaxObjectSizes: maxSizes,
		Breakers:       breakers,
	}, nil
}

// ProvideBreakerRegistry assembles the watchdog's breaker registry from the
// database circuit breaker and the per-backend breakers produced during
// backend initialization. Centralizing membership here keeps the watchdog
// itself free of type-assertions and keeps DI as the single wiring point.
func ProvideBreakerRegistry(i do.Injector) (*breaker.Registry, error) {
	dbCB, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	br, err := do.Invoke[*BackendsResult](i)
	if err != nil {
		return nil, err
	}
	reg := breaker.NewRegistry(dbCB)
	for _, b := range br.Breakers {
		reg.Register(b)
	}
	return reg, nil
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
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	metricsDeps, err := do.Invoke[metrics.Deps](i)
	if err != nil {
		return nil, err
	}
	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}

	return proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:          br.Backends,
		Stores:            stores,
		PendingEnabled:    cfg.WritePath.PendingPattern.IsEnabled(),
		Metrics:           metricsDeps,
		Dashboard:         stores,
		Order:             br.Order,
		CacheTTL:          cfg.CircuitBreaker.CacheTTL,
		BackendTimeout:    cfg.Server.BackendTimeout,
		UsageLimits:       br.UsageLimits,
		RoutingStrategy:   cfg.RoutingStrategy,
		ParallelBroadcast: cfg.CircuitBreaker.ParallelBroadcast,
		Encryptor:         enc,
		ObjectCache:       resolveOptionalCache(i),
		CounterBackend:    resolveOptionalCounterBackend(i),
		MaxObjectSizes:    br.MaxObjectSizes,
		AdmissionSem:      admissionSemFor(&cfg.Server),
		ReplicationFactor: replicationFactorFromInjector(i),
	}), nil
}

// resolveOptionalCounterBackend returns the configured Redis counter
// backend, or nil when Redis is disabled / not registered. The
// CounterBackend field on BackendManagerConfig accepts nil to mean
// "use the local counter backend".
func resolveOptionalCounterBackend(i do.Injector) counter.CounterBackend {
	rb, err := do.Invoke[*counter.RedisCounterBackend](i)
	if err != nil {
		return nil
	}
	return rb
}

// resolveOptionalCache returns the object data cache, or nil when
// caching is disabled / not registered. NewBackendManager treats nil as
// "object data caching is off" (read path bypasses the cache layer).
func resolveOptionalCache(i do.Injector) objcache.ObjectCache {
	c, err := do.Invoke[objcache.ObjectCache](i)
	if err != nil {
		return nil
	}
	return c
}

// admissionSemFor returns the shared admission semaphore sized per the
// server config. Reads/writes-split values take precedence over the
// merged MaxConcurrentRequests, matching the original switch's
// behaviour. Returns nil when no concurrency cap is configured.
func admissionSemFor(s *config.ServerConfig) chan struct{} {
	switch {
	case s.MaxConcurrentReads > 0 && s.MaxConcurrentWrites > 0:
		return make(chan struct{}, s.MaxConcurrentWrites)
	case s.MaxConcurrentRequests > 0:
		return make(chan struct{}, s.MaxConcurrentRequests)
	default:
		return nil
	}
}

// replicationFactorFromInjector returns a closure that lazily resolves
// the replicator from i and reads its hot-reloadable factor. Returns 0
// when replication is disabled or the replicator hasn't been registered
// yet (api mode).
func replicationFactorFromInjector(i do.Injector) func() int {
	return func() int {
		rep, err := do.Invoke[*worker.Replicator](i)
		if err != nil {
			return 0
		}
		if rc := rep.Config(); rc != nil {
			return rc.Factor
		}
		return 0
	}
}


// -------------------------------------------------------------------------
// BACKGROUND SERVICE PROVIDERS
// -------------------------------------------------------------------------

// lifecycleWorkerSet bundles the workers ProvideLifecycleManager registers
// services for. Resolved via resolveLifecycleWorkers so the provider body
// stays a flat sequence of registrations rather than a chain of error
// returns.
type lifecycleWorkerSet struct {
	cleanup       *worker.CleanupWorker
	rebalancer    *worker.Rebalancer
	replicator    *worker.Replicator
	overRep       *worker.OverReplicationCleaner
	scrubber      *worker.Scrubber
	pendingReaper *worker.PendingReaper // nil when the pending pattern is off
}

// resolveLifecycleWorkers invokes every worker the lifecycle manager
// registers a service for. drain.Manager is invoked too (not registered)
// so its DI side-effect  -  wiring itself into BackendManager  -  runs.
func resolveLifecycleWorkers(i do.Injector) (lifecycleWorkerSet, error) {
	var ws lifecycleWorkerSet
	var err error
	if ws.cleanup, err = do.Invoke[*worker.CleanupWorker](i); err != nil {
		return ws, err
	}
	if ws.rebalancer, err = do.Invoke[*worker.Rebalancer](i); err != nil {
		return ws, err
	}
	if ws.replicator, err = do.Invoke[*worker.Replicator](i); err != nil {
		return ws, err
	}
	if ws.overRep, err = do.Invoke[*worker.OverReplicationCleaner](i); err != nil {
		return ws, err
	}
	if ws.scrubber, err = do.Invoke[*worker.Scrubber](i); err != nil {
		return ws, err
	}
	if _, err = do.Invoke[*drain.Manager](i); err != nil {
		return ws, err
	}
	// PendingReaper provider returns (nil, nil) when the feature is off.
	ws.pendingReaper, _ = do.Invoke[*worker.PendingReaper](i)
	return ws, nil
}

// registerWorkerServices registers the worker-mode lifecycle services on
// sm. Pulled out of ProvideLifecycleManager so that function stays under
// the cognitive-complexity ceiling.
func registerWorkerServices(sm *lifecycle.Manager, mgr *proxy.BackendManager, ws lifecycleWorkerSet, locker core.AdvisoryLocker, cfg *config.Config) {
	sm.Register("multipart-cleanup", NewMultipartCleanupService(mgr, locker, cfg.CleanupQueue.MultipartStaleTimeout))
	sm.Register("cleanup-queue", NewCleanupQueueService(ws.cleanup, locker))
	if svc := NewPendingReaperService(ws.pendingReaper, locker, cfg.WritePath.PendingPattern.ReaperTick); svc != nil {
		sm.Register("pending-reaper", svc)
	}
	sm.Register("rebalancer", NewRebalancerService(mgr, ws.rebalancer, locker))
	sm.Register("replicator", NewReplicatorService(mgr, ws.replicator, locker))
	sm.Register("over-replication", NewOverReplicationService(mgr, ws.overRep, locker))
	sm.Register("lifecycle", NewLifecycleService(mgr, locker))
	sm.Register("scrubber", NewScrubberService(ws.scrubber, locker))
}

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
	registry, err := do.Invoke[*breaker.Registry](i)
	if err != nil {
		return nil, err
	}
	locker, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	mode, err := do.InvokeNamed[string](i, "mode")
	if err != nil {
		return nil, err
	}

	sm := lifecycle.NewManager()
	sm.Register("usage-flush", NewUsageFlushService(manager, locker))
	sm.Register("cb-watchdog", NewCircuitBreakerWatchdog(registry))

	if mode != "worker" && mode != "all" {
		return sm, nil
	}

	ws, err := resolveLifecycleWorkers(i)
	if err != nil {
		return nil, err
	}
	registerWorkerServices(sm, manager, ws, locker, cfg)

	if cfg.Reconcile.Enabled {
		reconciler, err := do.Invoke[*worker.Reconciler](i)
		if err != nil {
			return nil, err
		}
		sm.Register("reconcile", NewReconcileService(reconciler, locker, cfg.Reconcile.Interval))
	}
	if notifier, err := do.Invoke[*notify.Notifier](i); err == nil {
		sm.Register("notifications", notifier)
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
	adminHandler, err := do.Invoke[*admin.Handler](i)
	if err != nil {
		return nil, err
	}
	rebalancer, err := do.Invoke[*worker.Rebalancer](i)
	if err != nil {
		return nil, err
	}
	overRep, err := do.Invoke[*worker.OverReplicationCleaner](i)
	if err != nil {
		return nil, err
	}

	return ui.New(&ui.Deps{
		BackendOps:    manager,
		Objects:       manager.ObjectManager,
		Rebalancer:    rebalancer,
		OverRep:       overRep,
		AdminHandler:  adminHandler,
		DBHealthy:     cb.IsHealthy,
		Cfg:           cfg,
		LogBuffer:     logBuffer,
		LoginThrottle: loginThrottle,
	}), nil
}

// resolveOptionalEncryptor returns the live *encryption.Encryptor when
// encryption is enabled, or nil otherwise. A configured-but-failing
// encryptor surfaces as an error so the admin handler doesn't quietly
// run without encryption support.
func resolveOptionalEncryptor(i do.Injector, enabled bool) (*encryption.Encryptor, error) {
	if !enabled {
		return nil, nil //nolint:nilnil // encryption disabled is a valid state
	}
	e, err := do.Invoke[*encryption.Encryptor](i)
	if err != nil {
		return nil, fmt.Errorf("encryption enabled but encryptor failed to initialize: %w", err)
	}
	return e, nil
}

// invokeOptional resolves a provider that may not be registered, returning
// the zero value of T when the service is absent. Used for admin handler
// dependencies that only register under specific modes/features.
func invokeOptional[T any](i do.Injector) T {
	v, _ := do.Invoke[T](i)
	return v
}

// adminHandlerRequiredDeps bundles the required dependencies of
// ProvideAdminHandler so the provider body stays under the cognitive-
// complexity ceiling. Optional dependencies are resolved separately via
// invokeOptional.
type adminHandlerRequiredDeps struct {
	cfg        *config.Config
	manager    *proxy.BackendManager
	cb         *breaker.CircuitBreaker
	encAdmin   core.EncryptionAdmin
	logLevel   *slog.LevelVar
	enc        *encryption.Encryptor
	stores     core.MetadataStore
	replicator *worker.Replicator
	overRep    *worker.OverReplicationCleaner
	scrubber   *worker.Scrubber
	drain      *drain.Manager
}

// resolveAdminHandlerRequiredDeps invokes every required dependency the
// admin handler needs and bails on the first error.
func resolveAdminHandlerRequiredDeps(i do.Injector) (adminHandlerRequiredDeps, error) {
	var d adminHandlerRequiredDeps
	var err error
	if d.cfg, err = do.Invoke[*config.Config](i); err != nil {
		return d, err
	}
	if d.manager, err = do.Invoke[*proxy.BackendManager](i); err != nil {
		return d, err
	}
	if d.cb, err = do.Invoke[*breaker.CircuitBreaker](i); err != nil {
		return d, err
	}
	if d.encAdmin, err = do.Invoke[core.EncryptionAdmin](i); err != nil {
		return d, err
	}
	if d.logLevel, err = do.Invoke[*slog.LevelVar](i); err != nil {
		return d, err
	}
	if d.enc, err = resolveOptionalEncryptor(i, d.cfg.Encryption.Enabled); err != nil {
		return d, err
	}
	if d.stores, err = do.Invoke[core.MetadataStore](i); err != nil {
		return d, err
	}
	if d.replicator, err = do.Invoke[*worker.Replicator](i); err != nil {
		return d, err
	}
	if d.overRep, err = do.Invoke[*worker.OverReplicationCleaner](i); err != nil {
		return d, err
	}
	if d.scrubber, err = do.Invoke[*worker.Scrubber](i); err != nil {
		return d, err
	}
	if d.drain, err = do.Invoke[*drain.Manager](i); err != nil {
		return d, err
	}
	return d, nil
}

// ProvideAdminHandler creates the admin API handler.
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
	d, err := resolveAdminHandlerRequiredDeps(i)
	if err != nil {
		return nil, err
	}
	adminToken := d.cfg.UI.AdminToken
	if adminToken == "" {
		adminToken = d.cfg.UI.AdminKey
	}
	return admin.New(&admin.Deps{
		BackendOps:        d.manager,
		Replicator:        d.replicator,
		OverRep:           d.overRep,
		Drain:             d.drain,
		Scrubber:          d.scrubber,
		Lifecycle:         d.stores,
		DBHealthy:         d.cb.IsHealthy,
		Encryption:        d.encAdmin,
		Objects:           d.stores,
		Cleanup:           d.stores,
		Encryptor:         d.enc,
		Reconciler:        invokeOptional[*worker.Reconciler](i),
		MultipartBackfill: invokeOptional[*worker.MultipartBackfill](i),
		Token:             adminToken,
		LogLevel:          d.logLevel,
	}), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	stores, err := do.Invoke[core.MetadataStore](i)
	if err != nil {
		return nil, err
	}
	return notify.NewNotifier(&cfg.Notifications, stores), nil
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
