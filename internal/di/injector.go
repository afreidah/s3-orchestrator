// -------------------------------------------------------------------------------
// Dependency Injection - Top-Level Injector and Registration Order
//
// Author: Alex Freidah
//
// NewInjector creates the DI container and registers every provider the
// service needs. Providers are lazy - nothing is constructed until the
// corresponding do.Invoke call. Optional components (encryption, cache,
// Redis, notifications) register only when enabled in config; do.Invoke
// returns an error for disabled services, which callers use to detect
// absence.
//
// Narrow-role store providers are registered one per role (ObjectStore,
// QuotaStore, CleanupStore, ...) so consumers can ask only for the slice
// they actually use. Each narrow provider wraps the concrete *store.Store
// with the per-role CB decorator. No consumer ever sees a composed "god
// interface" - that type no longer exists.
//
// Non-DI packages (internal/*, internal/transport/*) never import samber/do.
// Constructors keep explicit parameters; only this package and cmd/ touch
// the injector.
//
// Provider bodies live in focused sibling files:
//   - store.go     (database, role aliases, metrics deps, instance id)
//   - backend.go   (S3 backends, breaker registry, manager, optional features)
//   - workers.go   (rebalancer, replicator, cleanup, scrubber, ...)
//   - lifecycle.go (lifecycle.Manager and service registration)
//   - transport.go (S3, admin, UI, notifier, rate limiter)
//   - optional.go  (invokeOptional + resolveOptional* helpers)
//   - services.go  (lifecycle Runner wrappers - locked-ticker background jobs)
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
	// PendingReaper is only registered when the pending-write pattern
	// is enabled (#830). Optional[*worker.PendingReaper] then reports
	// Disabled when the feature is off, Applied when it constructs
	// cleanly, and Failed when registration happened but construction
	// errored - three distinct states instead of the previous (nil,nil)
	// conflation.
	if cfg.WritePath.PendingPattern.IsEnabled() {
		do.Provide(inj, ProvidePendingReaper)
	}
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
// AUDIT WIRING
// -------------------------------------------------------------------------

// WireAuditMetrics connects the audit event counter to Prometheus. Called
// from the main binary during startup, outside the injector.
func WireAuditMetrics() {
	audit.SetOnEvent(func(event string) {
		telemetry.AuditEventsTotal.WithLabelValues(event).Inc()
	})
}
