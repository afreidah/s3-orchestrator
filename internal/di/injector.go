// -------------------------------------------------------------------------------
// Dependency Injection - Top-Level Injector and Registration Order
//
// Author: Alex Freidah
//
// NewInjector creates the DI container and delegates the per-domain
// provider registrations to the grouped helpers below. Providers are
// lazy — nothing is constructed until the matching do.Invoke call.
// Optional components (encryption, cache, Redis, notifications, UI,
// admin, flight recorder) register only when enabled in config;
// do.Invoke returns an error for disabled services, which callers use
// to detect absence.
// -------------------------------------------------------------------------------

package di

import (
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// InjectorDeps groups the values NewInjector seeds the container with.
type InjectorDeps struct {
	Config    *config.Config
	Mode      config.Mode
	LogLevel  *slog.LevelVar
	LogBuffer *telemetry.LogBuffer
}

// NewInjector creates the DI container and registers every provider
// the service needs. The body is intentionally a flat sequence of
// per-domain registration calls so a contributor can see the full
// dependency graph in one screen; each helper below owns the
// providers for its domain.
func NewInjector(deps InjectorDeps) do.Injector {
	inj := do.New()

	registerValues(inj, deps.Config, deps.Mode, deps.LogLevel, deps.LogBuffer)
	registerInfrastructure(inj)
	registerBackendStack(inj)
	registerWorkers(inj, deps.Config, deps.Mode)
	registerTransport(inj)
	registerOptionalFeatures(inj, deps.Config)

	return inj
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// registerValues seeds the injector with already-constructed values
// (config, mode, log level, log buffer) so providers can resolve them
// via do.Invoke without re-reading the YAML on every call.
func registerValues(inj do.Injector, cfg *config.Config, mode config.Mode, logLevel *slog.LevelVar, logBuffer *telemetry.LogBuffer) {
	do.ProvideValue(inj, cfg)
	do.ProvideNamedValue(inj, "mode", mode)
	do.ProvideValue(inj, logLevel)
	do.ProvideValue(inj, logBuffer)
}

// registerInfrastructure wires the always-on persistence,
// observability, and identity primitives every other provider builds
// on (metadata store, lifecycle / encryption admin views, notification
// outbox, database circuit breaker, instance id, metric deps).
func registerInfrastructure(inj do.Injector) {
	do.Provide(inj, provideMetadataStore)
	do.Provide(inj, ProvideDatabaseBreaker)
	do.Provide(inj, ProvideInstanceID)

	// Narrow role views of the wide metadata store: do.MustAs aliases the
	// concrete under each consumer interface, and verifies the cast at
	// registration instead of at first resolve.
	do.MustAs[metadataStore, core.AdvisoryLocker](inj)
	do.MustAs[metadataStore, core.LifecycleAdmin](inj)
	do.MustAs[metadataStore, core.EncryptionAdmin](inj)
	do.MustAs[metadataStore, core.NotificationOutbox](inj)
	do.MustAs[metadataStore, metrics.Deps](inj)
}

// registerBackendStack wires the storage-fleet composition objects:
// per-backend clients, the shared circuit-breaker registry, and each
// collaborator that routes, replicates or drains across them.
func registerBackendStack(inj do.Injector) {
	do.Provide(inj, ProvideBackends)
	do.Provide(inj, ProvideBreakerRegistry)
	do.Provide(inj, ProvideBackendRuntime)
	do.Provide(inj, ProvideIntegrityConfig)
	do.Provide(inj, ProvideWriteCoordinator)
	do.Provide(inj, ProvideDetachedUploads)
	do.Provide(inj, ProvideMultipartManager)
	do.Provide(inj, ProvideObjectManager)
	do.Provide(inj, ProvideDashboardAggregator)
	do.Provide(inj, ProvideExpiryManager)
	do.Provide(inj, ProvideReconcileManager)
	do.Provide(inj, ProvideUsageService)
}

// registerWorkers wires the background workers and the drain manager.
// PendingReaper and Reconciler register only when their feature is on
// (pending-write pattern enabled / worker-side mode).
func registerWorkers(inj do.Injector, cfg *config.Config, mode config.Mode) {
	do.Provide(inj, ProvideRebalancer)
	do.Provide(inj, ProvideReplicator)
	do.Provide(inj, ProvideOverReplicationCleaner)
	do.Provide(inj, ProvideCleanupWorker)
	// Always registered: every write claims its bytes with an intent, so a
	// deployment without the reaper would accumulate rows that hold a backend's
	// headroom against writes that are never coming.
	do.Provide(inj, ProvidePendingReaper)
	do.Provide(inj, ProvideScrubber)
	do.Provide(inj, ProvideDrainManager)
	if mode.IsWorker() {
		do.Provide(inj, ProvideReconciler)
	}
}

// registerTransport wires the always-on HTTP-side providers: bucket
// auth, the S3 API server, the operations layer both operator surfaces
// call, and the lifecycle manager that supervises the background services
// registered above. Conditional transport surfaces (UI, admin) live in
// registerOptionalFeatures.
func registerTransport(inj do.Injector) {
	do.Provide(inj, ProvideBucketAuth)
	do.Provide(inj, ProvideCORS)
	do.Provide(inj, ProvideS3Server)
	do.Provide(inj, ProvideOps)
	do.Provide(inj, ProvideLifecycleManager)
}

// provideIf registers provider only when enabled. A disabled feature never
// reaches the injector, so do.Invoke returns a clear "not registered" error the
// caller can distinguish from a runtime resolution failure. It keeps
// registerOptionalFeatures a flat, scannable table instead of a stack of if
// blocks.
func provideIf[T any](inj do.Injector, enabled bool, provider func(do.Injector) (T, error)) {
	if enabled {
		do.Provide(inj, provider)
	}
}

// registerOptionalFeatures wires every provider whose registration is gated on
// a config flag. Reads top to bottom as "provide X when its flag is set".
func registerOptionalFeatures(inj do.Injector, cfg *config.Config) {
	do.Provide(inj, ProvideCodec)
	provideIf(inj, cfg.Encryption.Enabled, ProvideEncryptor)
	provideIf(inj, cfg.Encryption.Enabled, ProvideEncryptionProvider)
	provideIf(inj, cfg.Redis != nil, ProvideRedisCounterBackend)
	provideIf(inj, cfg.Cache.Enabled, ProvideObjectCache)
	provideIf(inj, cfg.RateLimit.Enabled, ProvideRateLimiter)
	provideIf(inj, cfg.UI.Enabled, ProvideLoginThrottle)
	provideIf(inj, cfg.UI.Enabled, ProvideUIHandler)
	provideIf(inj, cfg.UI.AdminKey != "", ProvideAdminHandler)
	provideIf(inj, len(cfg.Notifications.Endpoints) > 0, ProvideNotifier)
	provideIf(inj, cfg.Debug.FlightRecorder.Enabled, ProvideFlightRecorderService)
}

// WireAuditMetrics connects the audit event counter to Prometheus. Called
// from the main binary during startup, outside the injector.
func WireAuditMetrics() {
	audit.SetOnEvent(func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	})
}
