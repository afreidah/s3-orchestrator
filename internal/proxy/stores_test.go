// In-package test helpers for the proxy package. Lives in a _test.go
// file so it is excluded from production builds.
package proxy

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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
// composition-root metadata store, installs the drain manager via
// WireDrain, and returns the worker handles so in-package tests can
// reach specific instances after wiring. Production code resolves
// workers through DI; this helper exists so proxy tests can build a
// fully-wired fixture without standing up the injector. The stores
// argument carries the wide MetadataStore because the test helper is
// itself the composition root for the in-package tests; each worker
// constructor narrows it to its declared role-composite interface.
func wireWorkersForTest(m *BackendManager, stores core.MetadataStore) *testWorkers {
	w := &testWorkers{}
	w.Rebalancer = worker.NewRebalancer(m, stores)
	w.Replicator = worker.NewReplicator(m, stores)
	w.OverReplicationCleaner = worker.NewOverReplicationCleaner(m, stores)
	w.CleanupWorker = worker.NewCleanupWorker(m, stores, 10, "test-instance", 5*time.Minute)
	w.PendingReaper = worker.NewPendingReaper(m, stores, 0, 0, 0)
	w.Scrubber = worker.NewScrubber(m, stores, nil)
	m.WireDrain(drain.New(
		m,
		stores,
		stores,
		stores,
		m.MultipartManager.AbortMultipartUploadsOnBackend,
		w.CleanupWorker.ProcessCleanupQueue,
	))
	return w
}

// newTestBackendManager builds a *BackendManager from cfg and
// fatal-fails the test on construction error. Used by in-package proxy
// tests so each call site stays a single line after NewBackendManager
// picked up an error return.
func newTestBackendManager(t *testing.T, cfg *BackendManagerConfig) *BackendManager {
	t.Helper()
	if cfg != nil && cfg.Stores != nil {
		if cfg.Dashboard == nil {
			cfg.Dashboard = cfg.Stores
		}
		if cfg.Metrics == nil {
			cfg.Metrics = cfg.Stores
		}
	}
	return NewBackendManager(cfg)
}

// testStoresFromMock returns m typed as the wide metadata-store contract
// every consumer depends on. A no-op identity since proxy.Stores has been
// folded into core.MetadataStore; kept so existing call sites read the
// same way as the production DI wiring.
func testStoresFromMock(m core.MetadataStore) core.MetadataStore { return m }
