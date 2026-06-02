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

// Package di is the single wiring point for the orchestrator. It uses
// samber/do/v2 to register every store role, backend, worker, and
// transport handler as a lazy provider; consumers resolve their
// dependencies through the injector at the moment they need them.
package di

import (
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// NewInjector creates the DI container and registers every provider
// the service needs. The body is intentionally a flat sequence of
// per-domain registration calls so a contributor can see the full
// dependency graph in one screen; each helper below owns the
// providers for its domain.
func NewInjector(cfg *config.Config, mode config.Mode, logLevel *slog.LevelVar, logBuffer *telemetry.LogBuffer) do.Injector {
	inj := do.New()

	registerValues(inj, cfg, mode, logLevel, logBuffer)
	registerInfrastructure(inj)
	registerBackendStack(inj)
	registerWorkers(inj, cfg, mode)
	registerTransport(inj)
	registerOptionalFeatures(inj, cfg)

	return inj
}

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
	do.Provide(inj, ProvideMetadataStore)
	do.Provide(inj, ProvideLifecycleAdmin)
	do.Provide(inj, ProvideEncryptionAdmin)
	do.Provide(inj, ProvideNotificationOutbox)
	do.Provide(inj, ProvideDatabaseBreaker)
	do.Provide(inj, ProvideInstanceID)
	do.Provide(inj, ProvideMetricsDeps)
}

// registerBackendStack wires the storage-fleet root composition
// objects: per-backend clients, the shared circuit-breaker registry,
// and the BackendManager that orchestrates routing / replication /
// drain across them.
func registerBackendStack(inj do.Injector) {
	do.Provide(inj, ProvideBackends)
	do.Provide(inj, ProvideBreakerRegistry)
	do.Provide(inj, ProvideBackendManager)
}

// registerWorkers wires the background workers and the drain manager.
// PendingReaper and Reconciler register only when their feature is on
// (pending-write pattern enabled / worker-side mode). drain.Manager
// itself wires onto BackendManager via WireDrain so the eligibility
// filters see drain state.
func registerWorkers(inj do.Injector, cfg *config.Config, mode config.Mode) {
	do.Provide(inj, ProvideRebalancer)
	do.Provide(inj, ProvideReplicator)
	do.Provide(inj, ProvideOverReplicationCleaner)
	do.Provide(inj, ProvideCleanupWorker)
	if cfg.WritePath.PendingPattern.IsEnabled() {
		do.Provide(inj, ProvidePendingReaper)
	}
	do.Provide(inj, ProvideScrubber)
	do.Provide(inj, ProvideDrainManager)
	if mode.IsWorker() {
		do.Provide(inj, ProvideReconciler)
	}
}

// registerTransport wires the always-on HTTP-side providers: bucket
// auth, the S3 API server, and the lifecycle manager that supervises
// the background services registered above. Conditional transport
// surfaces (UI, admin) live in registerOptionalFeatures.
func registerTransport(inj do.Injector) {
	do.Provide(inj, ProvideBucketAuth)
	do.Provide(inj, ProvideS3Server)
	do.Provide(inj, ProvideLifecycleManager)
}

// registerOptionalFeatures wires every provider whose registration is
// gated on a config flag. Disabled features intentionally never reach
// the injector so do.Invoke returns a clear "not registered" error
// the caller can distinguish from a runtime resolution failure.
func registerOptionalFeatures(inj do.Injector, cfg *config.Config) {
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
	if cfg.Debug.FlightRecorder.Enabled {
		do.Provide(inj, ProvideFlightRecorderService)
	}
}

// WireAuditMetrics connects the audit event counter to Prometheus. Called
// from the main binary during startup, outside the injector.
func WireAuditMetrics() {
	audit.SetOnEvent(func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	})
}
