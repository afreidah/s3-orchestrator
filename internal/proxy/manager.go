// -------------------------------------------------------------------------------
// Manager - Multi-Backend Object Storage Manager
//
// Author: Alex Freidah
//
// Core type and constructor for the backend manager. Object CRUD operations are
// in manager_objects.go, multipart operations in manager_multipart.go, quota
// metrics in manager_metrics.go, rebalancing in rebalancer.go, and replication
// in replicator.go.
// -------------------------------------------------------------------------------

// Package proxy is the domain orchestration layer that coordinates
// multi-backend S3 storage. It routes writes, manages failover reads,
// handles multipart uploads, drains backends, and exposes dashboard data.
// Workers receive the Ops interface instead of direct access.
package proxy

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// BACKEND MANAGER
// -------------------------------------------------------------------------

// ManagerStores is the persistence surface BackendManager itself touches,
// which is now only usage: recomputing drifted byte counters and flushing
// usage deltas. Every other store role reaches its consumer directly through
// that consumer's own constructor.
type ManagerStores interface {
	core.QuotaStore
	core.UsageFlusher
}

// StorageDeps groups the backend-fleet topology: the set of object
// backends to route across and the deterministic per-strategy iteration
// order.
type StorageDeps struct {
	Backends map[string]backend.ObjectBackend
	Order    []string
}

// StoreDeps groups the persistence dependencies. Metadata stays as the
// wide core.MetadataStore because BackendManager is the proxy subtree's
// composition root — it routes the concrete store into the narrow
// interfaces each sub-manager declares. Dashboard is already narrow.
type StoreDeps struct {
	Metadata ManagerStores
}

// PolicyConfig groups runtime tunables that shape how the manager
// behaves across normal and degraded operation. None of these enable a
// feature; they configure existing behavior.
type PolicyConfig struct {
	BackendTimeout time.Duration
	CacheTTL       time.Duration
	UsageLimits    map[string]core.UsageLimits
	// RoutingStrategy selects write-target ordering: pack vs spread.
	RoutingStrategy config.RoutingStrategy
	// ParallelBroadcast fans out reads in parallel during degraded mode.
	ParallelBroadcast bool
	// DegradedBroadcastParallelism caps concurrent probes during a
	// parallel degraded-mode broadcast. 0 = no cap (every backend
	// probed at once, the historical behaviour).
	DegradedBroadcastParallelism int
	// DisableDegradedReads opts the read path out of broadcasting on DB outage.
	DisableDegradedReads bool
	// PendingEnabled toggles the PUT-before-COMMIT pending-row pattern
	// (write_path.pending_pattern.enabled). When false the manager skips
	// pending-intent inserts and pending-promotion paths and falls back
	// to the legacy cleanup-on-failure flow.
	PendingEnabled bool
	// MaxObjectSizes is the per-backend max object size in bytes (0 = unlimited).
	MaxObjectSizes map[string]int64
}

// FeatureDeps groups optional capabilities. Each field is nil-able and
// disables the corresponding feature when left zero.
// Codec is supplied whether or not Compression.Enabled, because objects
// already stored compressed have to stay readable after the feature is turned
// off; Compression governs only whether new writes are encoded.
type FeatureDeps struct {
	Encryptor      *encryption.Encryptor  // nil when encryption is disabled
	CounterBackend counter.CounterBackend // nil uses LocalCounterBackend
	ObjectCache    objcache.ObjectCache   // nil when object data caching is disabled
	Codec          object.ObjectCodec     // nil when compression is not wired
	Compression    config.CompressionConfig
}

// OperationalDeps groups telemetry, concurrency, and observability
// callbacks the manager exposes to operators and to long-running
// background services.
type OperationalDeps struct {
	Metrics metrics.Deps
	// AdmissionSem is the shared concurrency semaphore for write-class
	// traffic. In split mode (MaxConcurrentReads + MaxConcurrentWrites)
	// it is sized to MaxConcurrentWrites and is shared between HTTP
	// writes and all background workers; reads run on a separate sem
	// created in transport/httpserver/routes.go. In merged mode
	// (MaxConcurrentRequests only) it is the global pool for every HTTP
	// request and every background worker. nil disables admission entirely
	// (no cap installed). See admissionSemFor in internal/di/backend.go
	// for the sizing rules.
	AdmissionSem chan struct{}
	// ReplicationFactor is invoked by the metrics collector when refreshing
	// the under-replicated-objects gauge. Returns 0 when replication is
	// disabled. Lazy-evaluated so it can resolve the live replicator's
	// configured factor (which is hot-reloadable).
	ReplicationFactor func() int
}

// Collaborators groups the sub-managers built by the composition root and
// injected so the drain manager (which needs the write coordinator as its
// mover and the multipart abort hook) and the BackendManager share the
// same instances.
//
// Coord, Multipart, and IntegrityCfg are required: the drain manager and
// the BackendManager must hold the same coordinator, multipart manager,
// and integrity-config pointer. Drain is nil-able; the methods that
// consult it (FlushUsage, ClearDrainState, GetDashboardData) nil-guard
// the field.
type Collaborators struct {
	Coord        *writepath.Coordinator
	Multipart    *multipart.Manager
	Objects      *object.Manager
	Drain        *drain.Manager
	IntegrityCfg *syncutil.AtomicConfig[config.IntegrityConfig]
}

// BackendManagerConfig groups the constructor parameters by capability
// so contributors can see at a glance which fields belong together:
// topology, persistence, runtime policy, optional features, operational
// deps, and pre-built collaborators. Each sub-struct documents its own
// field semantics.
type BackendManagerConfig struct {
	Runtime       *infra.BackendRuntime // backend fleet/admission/usage/metrics infrastructure, built by the composition root
	Storage       StorageDeps
	Stores        StoreDeps
	Policies      PolicyConfig
	Features      FeatureDeps
	Operations    OperationalDeps
	Collaborators Collaborators
}

// BackendManager manages multiple storage backends with quota tracking.
// It holds the backend runtime (non-store infrastructure: backends,
// usage, admission, draining, metrics) as a named field reached via
// Runtime(), plus the per-role store views and hot-reloadable config.
// Store-touching write-path helpers are methods on *BackendManager
// (manager_writepath.go); pure infra primitives stay on the runtime.
//
// Workers (rebalancer, replicator, scrubber, ...) are resolved through
// DI at the call site rather than carried on the manager.
//
// The drain manager is an injected collaborator. It is nil-able; the
// methods that consult it (FlushUsage, ClearDrainState, GetDashboardData)
// nil-guard the field so a manager built without drain stays usable.
type BackendManager struct {
	runtime          *infra.BackendRuntime  // backend fleet/admission/usage/metrics; expose via Runtime()
	stores           ManagerStores          // narrow store-role view; see ManagerStores interface above
	coord            *writepath.Coordinator // shared write-path helpers (also held by objectManager and multipartManager)
	multipartManager *multipart.Manager     // multipart upload lifecycle; expose via Multipart()
	objectManager    *object.Manager        // CRUD, read failover, broadcast reads; expose via Objects()
	drainManager     *drain.Manager         // nil-able; expose via Drain()

	usageFlushCfg syncutil.AtomicConfig[config.UsageFlushConfig]
	integrityCfg  *syncutil.AtomicConfig[config.IntegrityConfig] // shared with objectManager
}

// Multipart returns the multipart upload lifecycle manager. Exposed so
// transport and DI callers can reach multipart functionality without
// touching the unexported field directly.
func (m *BackendManager) Multipart() *multipart.Manager { return m.multipartManager }

// Objects returns the object CRUD manager. Same accessor rationale as
// Multipart().
func (m *BackendManager) Objects() *object.Manager { return m.objectManager }

// Coordinator returns the shared write-path coordinator. Workers take it as
// their Placement, and returning the manager's own instance keeps them on the
// same pending-pattern setting the write path uses.
func (m *BackendManager) Coordinator() *writepath.Coordinator { return m.coord }

// Runtime returns the backend runtime so workers, drain, and transport
// can depend on it directly for fleet/admission/usage primitives.
func (m *BackendManager) Runtime() *infra.BackendRuntime { return m.runtime }

// Drain returns the drain manager, or nil when the manager was built
// without one. Callers that touch the result must nil-guard.
func (m *BackendManager) Drain() *drain.Manager { return m.drainManager }

// NewBackendManager constructs a BackendManager. Required dependencies
// (cfg, Stores, Dashboard, Metrics) panic via must.NotNil at
// construction so a wiring bug surfaces immediately at DI assembly
// rather than NPE'ing N call frames deep on the first request. Numeric
// config invariants (negative timeouts, ordering rules) are the config
// validator's responsibility; the constructor trusts the values it
// receives.
func NewBackendManager(cfg *BackendManagerConfig) *BackendManager {
	must.NotNil("cfg", cfg)
	must.NotNil("cfg.Runtime", cfg.Runtime)
	must.NotNil("cfg.Stores.Metadata", cfg.Stores.Metadata)

	collab := cfg.Collaborators
	must.NotNil("cfg.Collaborators.Coord", collab.Coord)
	must.NotNil("cfg.Collaborators.Multipart", collab.Multipart)
	must.NotNil("cfg.Collaborators.Objects", collab.Objects)
	must.NotNil("cfg.Collaborators.IntegrityCfg", collab.IntegrityCfg)

	stores := cfg.Stores
	c := cfg.Runtime

	return &BackendManager{
		runtime:          c,
		stores:           stores.Metadata,
		coord:            collab.Coord,
		multipartManager: collab.Multipart,
		objectManager:    collab.Objects,
		drainManager:     collab.Drain,
		integrityCfg:     collab.IntegrityCfg,
	}
}

// ClearDrainState removes all entries from the draining map. Used by tests
// to reset state between runs. No-op when the manager has no drain manager.
func (m *BackendManager) ClearDrainState() {
	if m.drainManager == nil {
		return
	}
	m.drainManager.ClearState()
}

// AdmissionSem returns the shared admission semaphore, or nil if none is
// configured. The HTTP admission controller should use this channel so that
// HTTP requests and background services share one concurrency budget.
func (m *BackendManager) AdmissionSem() chan struct{} {
	return m.runtime.AdmissionSem()
}

// Close stops every background cache eviction goroutine the manager
// owns: the object location cache and the multipart per-upload DEK
// cache. Safe to call multiple times.
func (m *BackendManager) Close() {
	m.objectManager.LocationCache().Close()
	if m.multipartManager != nil {
		m.multipartManager.Close()
	}
}

// RecordUsage increments the in-memory usage counters for a backend.
// Exposed for admin operations that bypass the normal manager request path.
func (m *BackendManager) RecordUsage(backendName string, apiCalls, egress, ingress int64) {
	m.runtime.Usage().Record(backendName, apiCalls, egress, ingress)
}

// UpdateUsageLimits replaces the per-backend usage limits. Safe to call
// concurrently with request handling.
func (m *BackendManager) UpdateUsageLimits(limits map[string]core.UsageLimits) {
	m.runtime.Usage().UpdateLimits(limits)
}

// FlushUsage flushes accumulated in-memory usage counters to the database.
// Backends that have completed draining are skipped because their DB
// records (including backend_usage) have been removed. When DrainManager
// has not been wired (tests that do not exercise drain behavior) the
// skip set is empty and every backend's counters flush.
func (m *BackendManager) FlushUsage(ctx context.Context) error {
	var skip map[string]bool
	if m.drainManager != nil {
		skip = m.drainManager.CompletedBackends()
	}
	return m.runtime.Usage().FlushUsage(ctx, m.stores, skip)
}

// RedisCounterConfigured returns true when the counter backend is a Redis
// backend, regardless of health status. Used by the flush service to decide
// whether an advisory lock is needed  -  the lock must be held even during
// fallback to prevent double-counting when Redis recovers mid-flush.
func (m *BackendManager) RedisCounterConfigured() bool {
	_, ok := m.runtime.Usage().Backend().(*counter.RedisCounterBackend)
	return ok
}

// -------------------------------------------------------------------------
// CONFIG ACCESSORS
// -------------------------------------------------------------------------

// SetUsageFlushConfig atomically stores the usage flush configuration.
func (m *BackendManager) SetUsageFlushConfig(cfg *config.UsageFlushConfig) {
	m.usageFlushCfg.Store(cfg)
}

// UsageFlushConfig returns the current usage flush configuration.
func (m *BackendManager) UsageFlushConfig() *config.UsageFlushConfig {
	return m.usageFlushCfg.Load()
}

// SetIntegrityConfig atomically stores the integrity configuration.
// The scrubber's own SetConfig is invoked separately by the caller
// (serve) because the scrubber is resolved through DI rather than held
// on the manager.
func (m *BackendManager) SetIntegrityConfig(cfg *config.IntegrityConfig) {
	m.integrityCfg.Store(cfg)
}

// IntegrityConfig returns the current integrity configuration.
func (m *BackendManager) IntegrityConfig() *config.IntegrityConfig {
	return m.integrityCfg.Load()
}

// NearUsageLimit returns true if any backend is approaching its usage limits.
func (m *BackendManager) NearUsageLimit(threshold float64) bool {
	return m.runtime.Usage().NearLimit(threshold)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// ReconcileUsage recomputes each backend's bytes_used counter from the object
// ledger, correcting drift in the incrementally maintained counter. Part of
// the BackendSyncer contract the reconciler drives every pass; also exposed to
// the admin reconcile-usage endpoint.
func (m *BackendManager) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	return m.stores.ReconcileUsage(ctx)
}

// -------------------------------------------------------------------------
// STORE-ROLE ACCESSORS
// -------------------------------------------------------------------------

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// -------------------------------------------------------------------------
// PASS-THROUGHS
// -------------------------------------------------------------------------

// UpdateQuotaMetrics forwards to the runtime. The usage-flush and
// reconcile services consume it alongside the manager's store-coupled
// helpers, so the manager exposes it as part of its orchestration surface.
func (m *BackendManager) UpdateQuotaMetrics(ctx context.Context) error {
	return m.runtime.UpdateQuotaMetrics(ctx)
}

// BackendOrder forwards to the runtime. The reconciler iterates the fleet
// in this order while reconciling backend state against the stores.
func (m *BackendManager) BackendOrder() []string {
	return m.runtime.BackendOrder()
}
