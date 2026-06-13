// In-package test helpers for the proxy package. Lives in a _test.go
// file so it is excluded from production builds.
package proxy

import (
	"log/slog"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// testWorkers bundles the workers wireWorkersForTest constructed. In-
// package proxy tests grab specific workers from this struct now that
// they are no longer fields on BackendManager.
type testWorkers struct {
	Rebalancer             *worker.Rebalancer
	Replicator             *worker.Replicator
	OverReplicationCleaner *worker.OverReplicationCleaner
	CleanupWorker          *worker.CleanupWorker
	PendingReaper          *worker.PendingReaper
	Scrubber               *worker.Scrubber
}

// wireWorkersForTest constructs every worker against the supplied
// composition-root metadata store, installs the drain manager on the
// BackendManager, and returns the worker handles so in-package tests can
// reach specific instances after wiring. Production code resolves
// workers through DI; this helper exists so proxy tests can build a
// fully-wired fixture without standing up the injector. The stores
// argument carries the wide MetadataStore because the test helper is
// itself the composition root for the in-package tests; each worker
// constructor narrows it to its declared role-composite interface.
func wireWorkersForTest(m *BackendManager, stores core.MetadataStore) *testWorkers {
	w := &testWorkers{}
	w.Rebalancer = worker.NewRebalancer(m.Runtime(), m, stores)
	w.Replicator = worker.NewReplicator(m.Runtime(), m, stores)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(m.Runtime(), m, stores)
	w.CleanupWorker = worker.NewCleanupWorker(m.Runtime(), stores, 10, "test-instance", 5*time.Minute)
	w.PendingReaper = worker.NewPendingReaper(m.Runtime(), m, stores, 0, 0, 0)
	w.Scrubber = worker.NewScrubber(m.Runtime(), m, stores, nil)
	dm := drain.New(
		m.Runtime(),
		m,
		stores,
		stores,
		stores,
		m.multipartManager.AbortMultipartUploadsOnBackend,
		w.CleanupWorker.ProcessCleanupQueue,
	)
	m.drainManager = dm
	m.Runtime().SetDrainChecker(dm)
	return w
}

// newTestBackendManager builds a *BackendManager from cfg and
// fatal-fails the test on construction error. Used by in-package proxy
// tests so each call site stays a single line after NewBackendManager
// picked up an error return.
func newTestBackendManager(t *testing.T, cfg *BackendManagerConfig) *BackendManager {
	t.Helper()
	if cfg != nil && cfg.Stores.Metadata != nil {
		if cfg.Stores.Dashboard == nil {
			cfg.Stores.Dashboard = cfg.Stores.Metadata
		}
		if cfg.Operations.Metrics == nil {
			cfg.Operations.Metrics = cfg.Stores.Metadata
		}
	}
	// This helper is the composition root for in-package tests, so it
	// assembles the backend runtime and shared collaborators the production
	// DI providers build when a test supplies the flat config instead.
	if cfg != nil && cfg.Runtime == nil {
		cfg.Runtime = testBackendRuntime(cfg)
	}
	if cfg != nil && cfg.Collaborators.Coord == nil {
		cfg.Collaborators = testCollaborators(cfg)
	}
	return NewBackendManager(cfg)
}

// testCollaborators builds the write coordinator, multipart manager, and
// shared integrity config the manager requires, mirroring the production
// DI providers. Drain is left nil; wireWorkersForTest installs it.
func testCollaborators(cfg *BackendManagerConfig) Collaborators {
	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(cfg.Runtime, cfg.Stores.Metadata, cfg.Policies.PendingEnabled)
	mp := multipart.New(cfg.Runtime, coord, cfg.Stores.Metadata, cfg.Features.Encryptor, cfg.Features.ObjectCache, time.Hour, integrityCfg)
	return Collaborators{Coord: coord, Multipart: mp, IntegrityCfg: integrityCfg}
}

// testBackendRuntime builds a *infra.BackendRuntime from the legacy flat
// fields of a test BackendManagerConfig, mirroring di.ProvideBackendRuntime.
func testBackendRuntime(cfg *BackendManagerConfig) *infra.BackendRuntime {
	backendNames := make([]string, 0, len(cfg.Storage.Backends))
	for name := range cfg.Storage.Backends {
		backendNames = append(backendNames, name)
	}
	counters := cfg.Features.CounterBackend
	if counters == nil {
		counters = counter.NewLocalCounterBackend(backendNames)
	}
	usage := counter.NewUsageTracker(counters, cfg.Policies.UsageLimits)
	rt := infra.New(&infra.Config{
		Backends:        cfg.Storage.Backends,
		Order:           cfg.Storage.Order,
		BackendTimeout:  cfg.Policies.BackendTimeout,
		Usage:           usage,
		RoutingStrategy: cfg.Policies.RoutingStrategy,
		MaxObjectSizes:  cfg.Policies.MaxObjectSizes,
		AdmissionSem:    cfg.Operations.AdmissionSem,
		Log:             slog.Default().With(logfmt.Component("backend_manager")),
	})
	if cfg.Operations.Metrics != nil {
		rt.SetMetricsCollector(metrics.New(cfg.Operations.Metrics, usage, backendNames, cfg.Operations.ReplicationFactor))
	}
	return rt
}

// testStoresFromMock returns m typed as the wide metadata-store contract
// every consumer depends on. A no-op identity since proxy.ManagerStores is
// satisfied by core.MetadataStore; kept so existing call sites read the
// same way as the production DI wiring.
func testStoresFromMock(m core.MetadataStore) core.MetadataStore { return m }
