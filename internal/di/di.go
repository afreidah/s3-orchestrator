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
	do.Provide(inj, provideConcreteStore)
	do.Provide(inj, ProvideLifecycleAdmin)
	do.Provide(inj, ProvideEncryptionAdmin)
	do.Provide(inj, ProvideNotificationOutbox)
	do.Provide(inj, ProvideDatabaseBreaker)

	// Narrow per-role store providers  -  each wraps provideConcreteStore's
	// value with its per-role CB decorator.
	do.Provide(inj, ProvideObjectStore)
	do.Provide(inj, ProvideQuotaStore)
	do.Provide(inj, ProvideMultipartStore)
	do.Provide(inj, ProvideReplicationStore)
	do.Provide(inj, ProvideCleanupStore)
	do.Provide(inj, ProvidePendingStore)
	do.Provide(inj, ProvideIntegrityStore)
	do.Provide(inj, ProvideExpiredObjectsLister)
	do.Provide(inj, ProvideBackendLifecycleStore)
	do.Provide(inj, ProvideDashboardStore)
	do.Provide(inj, ProvideUsageFlusher)
	do.Provide(inj, ProvideAdvisoryLocker)
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

// concreteStore collects every role interface satisfied by the driver-
// level store without introducing a user-facing composed type. Declared
// unexported and scoped to this package  -  callers outside di never see it.
// Both PostgreSQL *postgres.Store and SQLite *sqlite.Store satisfy this.
type concreteStore interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
	core.LifecycleAdmin
	core.EncryptionAdmin
	core.NotificationOutbox
	metricsDeps
}

// metricsDeps names the five methods proxy.MetricsDeps requires. Declared
// here (not exported) because it only exists so concreteStore satisfies the
// proxy-owned MetricsDeps contract structurally.
type metricsDeps interface {
	GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error)
}

// provideConcreteStore creates the concrete driver store for the configured
// driver, runs migrations, and syncs quota limits. Returned as an
// unexported composite; no call site outside this package references it
// directly. Narrow per-role providers extract the role they need from this
// single concrete value.
func provideConcreteStore(i do.Injector) (concreteStore, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	cs, err := openStore(ctx, &cfg.Database)
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

// extractAdminRole resolves the concreteStore from the injector and
// narrows it to the requested admin-role interface T. Each ProvideX admin
// extractor below is a one-line wrapper around this helper - inlining the
// three-line resolve+narrow body in each one would produce identical
// implementations and trigger duplicate-code lints.
func extractAdminRole[T any](i do.Injector) (T, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		var zero T
		return zero, err
	}
	return any(cs).(T), nil
}

// ProvideLifecycleAdmin exposes the LifecycleAdmin role from the concrete
// store. Used at boot/shutdown for migrations, schema checks, and Close.
func ProvideLifecycleAdmin(i do.Injector) (core.LifecycleAdmin, error) {
	return extractAdminRole[core.LifecycleAdmin](i)
}

// ProvideEncryptionAdmin exposes the EncryptionAdmin role from the
// concrete store. Used by the admin HTTP handler for key rotation and
// encrypt/decrypt batch operations.
func ProvideEncryptionAdmin(i do.Injector) (core.EncryptionAdmin, error) {
	return extractAdminRole[core.EncryptionAdmin](i)
}

// ProvideNotificationOutbox exposes the NotificationOutbox role from the
// concrete store. Used by the notifier worker for durable webhook delivery.
func ProvideNotificationOutbox(i do.Injector) (core.NotificationOutbox, error) {
	return extractAdminRole[core.NotificationOutbox](i)
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
func openStore(ctx context.Context, dbCfg *config.DatabaseConfig) (concreteStore, error) {
	switch dbCfg.Driver {
	case "postgres":
		return postgres.NewStore(ctx, dbCfg)
	case "sqlite":
		return sqlitestore.NewStore(ctx, dbCfg)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q", dbCfg.Driver)
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
func ProvideObjectStore(i do.Injector) (core.ObjectStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBObjectStore(cs, cb), nil
}

// ProvideQuotaStore registers a CB-protected QuotaStore view.
func ProvideQuotaStore(i do.Injector) (core.QuotaStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBQuotaStore(cs, cb), nil
}

// ProvideMultipartStore registers a CB-protected MultipartStore view.
func ProvideMultipartStore(i do.Injector) (core.MultipartStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBMultipartStore(cs, cb), nil
}

// ProvideReplicationStore registers a CB-protected ReplicationStore view.
func ProvideReplicationStore(i do.Injector) (core.ReplicationStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBReplicationStore(cs, cb), nil
}

// ProvideCleanupStore registers a CB-protected CleanupStore view.
func ProvideCleanupStore(i do.Injector) (core.CleanupStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBCleanupStore(cs, cb), nil
}

// ProvidePendingStore registers a CB-protected PendingStore view used by the
// write path's PUT-before-COMMIT pending-row pattern. Returns nil when the
// operator disables the pattern via write_path.pending_pattern.enabled=false;
// the write path's nil check falls back to the legacy cleanup-on-failure
// behaviour in that case.
func ProvidePendingStore(i do.Injector) (core.PendingStore, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	if !cfg.WritePath.PendingPattern.IsEnabled() {
		return nil, nil //nolint:nilnil // intentional: nil signals feature off, caller branches on it
	}
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBPendingStore(cs, cb), nil
}

// ProvideIntegrityStore registers a CB-protected IntegrityStore view.
func ProvideIntegrityStore(i do.Injector) (core.IntegrityStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBIntegrityStore(cs, cb), nil
}

// ProvideExpiredObjectsLister registers a CB-protected ExpiredObjectsLister view.
func ProvideExpiredObjectsLister(i do.Injector) (core.ExpiredObjectsLister, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBExpiredObjectsLister(cs, cb), nil
}

// ProvideBackendLifecycleStore registers a CB-protected BackendLifecycleStore view.
func ProvideBackendLifecycleStore(i do.Injector) (core.BackendLifecycleStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBBackendLifecycleStore(cs, cb), nil
}

// ProvideDashboardStore registers a CB-protected DashboardStore view.
func ProvideDashboardStore(i do.Injector) (core.DashboardStore, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBDashboardStore(cs, cb), nil
}

// ProvideUsageFlusher registers a CB-protected UsageFlusher view.
func ProvideUsageFlusher(i do.Injector) (core.UsageFlusher, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	cb, err := do.Invoke[*breaker.CircuitBreaker](i)
	if err != nil {
		return nil, err
	}
	return store.NewCBUsageFlusher(cs, cb), nil
}

// ProvideAdvisoryLocker registers a pass-through AdvisoryLocker  -  advisory
// locks bypass the breaker (see internal/store/cb_lock.go).
func ProvideAdvisoryLocker(i do.Injector) (core.AdvisoryLocker, error) {
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	return store.NewAdvisoryLocker(cs), nil
}

// metricsDepsAdapter composes the narrow roles metrics.Collector queries.
// Struct embedding promotes each method to the top-level type, so the
// adapter structurally satisfies metrics.Deps without inventing a
// composed interface.
type metricsDepsAdapter struct {
	core.DashboardStore   // GetQuotaStats, GetObjectCounts, GetActiveMultipartCounts, GetUsageForPeriod
	core.ReplicationStore // GetUnderReplicatedObjects (among others)
}

// ProvideMetricsDeps builds the adapter metrics.Collector uses to refresh
// Prometheus gauges.
func ProvideMetricsDeps(i do.Injector) (metrics.Deps, error) {
	dash, err := do.Invoke[core.DashboardStore](i)
	if err != nil {
		return nil, err
	}
	repl, err := do.Invoke[core.ReplicationStore](i)
	if err != nil {
		return nil, err
	}
	return &metricsDepsAdapter{DashboardStore: dash, ReplicationStore: repl}, nil
}

// -------------------------------------------------------------------------
// WORKER PROVIDERS
//
// Each worker is its own DI service. Providers invoke *proxy.BackendManager
// (which satisfies worker.Ops / CleanupOps / ScrubberOps via promoted
// backendCore methods plus its own write-path helpers) and the per-worker
// store role. The worker.Rebalancer/Replicator/OverReplicationCleaner take
// compound store contracts; the providers build the embed-shaped adapter
// inline rather than declaring a named struct in the production package.
// -------------------------------------------------------------------------

// rebalancerStoreAdapter satisfies worker.RebalancerStore.
type rebalancerStoreAdapter struct {
	core.ObjectStore
	core.QuotaStore
}

// replicatorStoreAdapter satisfies worker.ReplicatorStore.
type replicatorStoreAdapter struct {
	core.ObjectStore
	core.ReplicationStore
	core.QuotaStore
}

// overReplicationStoreAdapter satisfies worker.OverReplicationStore.
type overReplicationStoreAdapter struct {
	core.ReplicationStore
	core.QuotaStore
}

// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	obj, err := do.Invoke[core.ObjectStore](i)
	if err != nil {
		return nil, err
	}
	q, err := do.Invoke[core.QuotaStore](i)
	if err != nil {
		return nil, err
	}
	rb := worker.NewRebalancer(mgr, &rebalancerStoreAdapter{ObjectStore: obj, QuotaStore: q})
	mgr.Rebalancer = rb
	return rb, nil
}

// ProvideReplicator constructs the replication worker.
func ProvideReplicator(i do.Injector) (*worker.Replicator, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	obj, err := do.Invoke[core.ObjectStore](i)
	if err != nil {
		return nil, err
	}
	repl, err := do.Invoke[core.ReplicationStore](i)
	if err != nil {
		return nil, err
	}
	q, err := do.Invoke[core.QuotaStore](i)
	if err != nil {
		return nil, err
	}
	rp := worker.NewReplicator(mgr, &replicatorStoreAdapter{ObjectStore: obj, ReplicationStore: repl, QuotaStore: q})
	mgr.Replicator = rp
	return rp, nil
}

// ProvideOverReplicationCleaner constructs the over-replication cleanup worker.
func ProvideOverReplicationCleaner(i do.Injector) (*worker.OverReplicationCleaner, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	repl, err := do.Invoke[core.ReplicationStore](i)
	if err != nil {
		return nil, err
	}
	q, err := do.Invoke[core.QuotaStore](i)
	if err != nil {
		return nil, err
	}
	or := worker.NewOverReplicationCleaner(mgr, &overReplicationStoreAdapter{ReplicationStore: repl, QuotaStore: q})
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
	cleanup, err := do.Invoke[core.CleanupStore](i)
	if err != nil {
		return nil, err
	}
	concurrency := cfg.CleanupQueue.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	cw := worker.NewCleanupWorker(mgr, cleanup, concurrency)
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
	pending, err := do.Invoke[core.PendingStore](i)
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
	integrity, err := do.Invoke[core.IntegrityStore](i)
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

// ProvideDrainManager constructs the drain manager. Depends on
// BackendManager (drain.Core seam), the cleanup worker (for the
// cleanup-queue flush before backend deletion), and the per-worker store
// roles drain pulls directly.
func ProvideDrainManager(i do.Injector) (*drain.Manager, error) {
	mgr, err := do.Invoke[*proxy.BackendManager](i)
	if err != nil {
		return nil, err
	}
	cleanup, err := do.Invoke[*worker.CleanupWorker](i)
	if err != nil {
		return nil, err
	}
	obj, err := do.Invoke[core.ObjectStore](i)
	if err != nil {
		return nil, err
	}
	q, err := do.Invoke[core.QuotaStore](i)
	if err != nil {
		return nil, err
	}
	blc, err := do.Invoke[core.BackendLifecycleStore](i)
	if err != nil {
		return nil, err
	}
	dm := drain.New(
		mgr,
		obj,
		q,
		blc,
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
	stores, err := resolveProxyStores(i)
	if err != nil {
		return nil, err
	}
	dash, err := do.Invoke[core.DashboardStore](i)
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
		Metrics:           metricsDeps,
		Dashboard:         dash,
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

// resolveProxyStores assembles the proxy.Stores bag by invoking each narrow
// role provider. Defined here so ProvideBackendManager stays readable.
func resolveProxyStores(i do.Injector) (proxy.Stores, error) {
	obj, err := do.Invoke[core.ObjectStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	q, err := do.Invoke[core.QuotaStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	mp, err := do.Invoke[core.MultipartStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	rep, err := do.Invoke[core.ReplicationStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	cu, err := do.Invoke[core.CleanupStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	pen, err := do.Invoke[core.PendingStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	ig, err := do.Invoke[core.IntegrityStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	lc, err := do.Invoke[core.ExpiredObjectsLister](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	blc, err := do.Invoke[core.BackendLifecycleStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	dash, err := do.Invoke[core.DashboardStore](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	uf, err := do.Invoke[core.UsageFlusher](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	lock, err := do.Invoke[core.AdvisoryLocker](i)
	if err != nil {
		return proxy.Stores{}, err
	}
	return proxy.Stores{
		Object:           obj,
		Quota:            q,
		Multipart:        mp,
		Replication:      rep,
		Cleanup:          cu,
		Pending:          pen,
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
	locker, err := do.Invoke[core.AdvisoryLocker](i)
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

	bktNames := make([]string, len(cfg.Buckets))
	for idx, b := range cfg.Buckets {
		bktNames[idx] = b.Name
	}
	reconciler := worker.NewReconciler(manager, bktNames)
	do.ProvideValue(i, reconciler)
	if cfg.Reconcile.Enabled {
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

// adminHandlerDeps groups the worker handles + narrow store roles
// ProvideAdminHandler resolves so the provider body stays compact.
type adminHandlerDeps struct {
	replicator *worker.Replicator
	overRep    *worker.OverReplicationCleaner
	scrubber   *worker.Scrubber
	drain      *drain.Manager
	objects    core.ObjectStore
	cleanup    core.CleanupStore
	lifecycle  core.BackendLifecycleStore
}

// resolveAdminHandlerDeps invokes the worker + store dependencies the
// admin handler needs. Each invocation may fail (e.g. provider missing),
// so the helper bails on the first error.
func resolveAdminHandlerDeps(i do.Injector) (adminHandlerDeps, error) {
	var d adminHandlerDeps
	var err error
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
	if d.objects, err = do.Invoke[core.ObjectStore](i); err != nil {
		return d, err
	}
	if d.cleanup, err = do.Invoke[core.CleanupStore](i); err != nil {
		return d, err
	}
	if d.lifecycle, err = do.Invoke[core.BackendLifecycleStore](i); err != nil {
		return d, err
	}
	return d, nil
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
	encAdmin, err := do.Invoke[core.EncryptionAdmin](i)
	if err != nil {
		return nil, err
	}
	logLevel, err := do.Invoke[*slog.LevelVar](i)
	if err != nil {
		return nil, err
	}
	enc, err := resolveOptionalEncryptor(i, cfg.Encryption.Enabled)
	if err != nil {
		return nil, err
	}

	deps, err := resolveAdminHandlerDeps(i)
	if err != nil {
		return nil, err
	}

	// Reconciler is optional: only registered in worker/all modes.
	var reconciler *worker.Reconciler
	if r, err := do.Invoke[*worker.Reconciler](i); err == nil {
		reconciler = r
	}

	adminToken := cfg.UI.AdminToken
	if adminToken == "" {
		adminToken = cfg.UI.AdminKey
	}

	return admin.New(&admin.Deps{
		BackendOps: manager,
		Replicator: deps.replicator,
		OverRep:    deps.overRep,
		Drain:      deps.drain,
		Scrubber:   deps.scrubber,
		Lifecycle:  deps.lifecycle,
		DBCB:       cb,
		Encryption: encAdmin,
		Objects:    deps.objects,
		Cleanup:    deps.cleanup,
		Encryptor:  enc,
		Reconciler: reconciler,
		Token:      adminToken,
		LogLevel:   logLevel,
	}), nil
}

// ProvideNotifier creates the webhook notification system.
func ProvideNotifier(i do.Injector) (*notify.Notifier, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	cs, err := do.Invoke[concreteStore](i)
	if err != nil {
		return nil, err
	}
	return notify.NewNotifier(&cfg.Notifications, cs), nil
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
