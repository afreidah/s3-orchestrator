// In-package test helpers for the proxy package. Lives in a _test.go
// file so it is excluded from production builds.
package proxy

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/multipart"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
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
	w.Rebalancer = worker.NewRebalancer(m.Runtime(), m.coord, stores)
	w.Replicator = worker.NewReplicator(m.Runtime(), m.coord, stores)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(m.Runtime(), m.coord, stores)
	w.CleanupWorker = worker.NewCleanupWorker(worker.CleanupWorkerDeps{Ops: m.Runtime(), Store: stores, Concurrency: 10, InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute})
	w.PendingReaper = worker.NewPendingReaper(worker.PendingReaperDeps{Ops: m.Runtime(), Placement: m.coord, Store: stores})
	w.Scrubber = worker.NewScrubber(worker.ScrubberDeps{Ops: m.Runtime(), Placement: m.coord, Store: stores})
	processCleanup := func(ctx context.Context) (int, int) {
		sum := w.CleanupWorker.ProcessCleanupQueue(ctx)
		return sum.Succeeded, sum.Failed
	}
	dm := drain.New(
		m.Runtime(),
		m.coord,
		stores,
		stores,
		stores,
		m.multipartManager.AbortMultipartUploadsOnBackend,
		processCleanup,
	)
	m.drainManager = dm
	m.Runtime().SetDrainChecker(dm)
	return w
}

// newPermissiveMock returns a union store mock answering every read with an
// empty result, so a BackendManager test states only the queries it asserts
// on. The composition root is what these tests exercise, so the store behind
// it stays out of the way.
func newPermissiveMock(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}

// newTestBackendManager builds a *BackendManager from cfg and
// fatal-fails the test on construction error. Used by in-package proxy
// tests so each call site stays a single line after NewBackendManager
// picked up an error return.
func newTestBackendManager(t *testing.T, cfg *BackendManagerConfig) *BackendManager {
	t.Helper()
	if cfg != nil && cfg.Stores.Metadata != nil && cfg.Operations.Metrics == nil {
		// The wide test store satisfies the metrics collector too; the narrow
		// StoreDeps field no longer carries enough to assume it.
		if m, ok := cfg.Stores.Metadata.(metrics.Deps); ok {
			cfg.Operations.Metrics = m
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
// testCollaborators builds the sub-managers a test manager needs. It recovers
// the wide store from the narrow StoreDeps field: BackendManager itself now
// takes only the usage surface, but the sub-managers still need their own
// roles, and in-package tests always supply a full mock.
func testCollaborators(cfg *BackendManagerConfig) Collaborators {
	wide, ok := cfg.Stores.Metadata.(core.MetadataStore)
	if !ok {
		panic("in-package tests must supply a full core.MetadataStore")
	}
	integrityCfg := &syncutil.AtomicConfig[config.IntegrityConfig]{}
	coord := writepath.New(cfg.Runtime, wide, cfg.Policies.PendingEnabled)
	mp := multipart.New(&multipart.Deps{
		Core:         cfg.Runtime,
		Coord:        coord,
		Stores:       wide,
		Encryptor:    cfg.Features.Encryptor,
		ObjectCache:  cfg.Features.ObjectCache,
		DEKCacheTTL:  time.Hour,
		IntegrityCfg: integrityCfg,
	})
	om := object.New(&object.Deps{
		Core:                         cfg.Runtime,
		BroadcastCore:                cfg.Runtime,
		Coord:                        coord,
		Stores:                       wide,
		Encryptor:                    cfg.Features.Encryptor,
		LocationCache:                object.NewLocationCache(cfg.Policies.CacheTTL),
		ObjectCache:                  cfg.Features.ObjectCache,
		ParallelBroadcast:            cfg.Policies.ParallelBroadcast,
		DegradedBroadcastParallelism: cfg.Policies.DegradedBroadcastParallelism,
		DisableDegradedReads:         cfg.Policies.DisableDegradedReads,
		IntegrityCfg:                 integrityCfg,
		BackendTimeout:               cfg.Policies.BackendTimeout,
	})
	return Collaborators{Coord: coord, Multipart: mp, Objects: om, IntegrityCfg: integrityCfg}
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
		rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
			Store:             cfg.Operations.Metrics,
			Usage:             usage,
			BackendNames:      backendNames,
			ReplicationFactor: cfg.Operations.ReplicationFactor,
		}))
	}
	return rt
}

// testStoresFromMock returns m typed as the wide metadata-store contract
// every consumer depends on. A no-op identity since proxy.ManagerStores is
// satisfied by core.MetadataStore; kept so existing call sites read the
// same way as the production DI wiring.
func testStoresFromMock(m core.MetadataStore) core.MetadataStore { return m }
