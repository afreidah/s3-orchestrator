// In-package test helpers for the proxy package. Lives in a _test.go
// file so it is excluded from production builds.
package proxy

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// rebalancerStoreT, replicatorStoreT, overReplicationStoreT mirror the
// adapter types worker constructors require. Declared here so proxy's
// own fixtures can wire workers without importing di.
type rebalancerStoreT struct {
	core.ObjectStore
	core.QuotaStore
}

type replicatorStoreT struct {
	core.ObjectStore
	core.ReplicationStore
	core.QuotaStore
}

type overReplicationStoreT struct {
	core.ReplicationStore
	core.QuotaStore
}

// wireWorkersForTest constructs all workers and attaches them to the
// manager, returning the same pointer for fluent chaining at the
// "return NewBackendManager(...)" sites in test fixtures.
func wireWorkersForTest(m *BackendManager) *BackendManager {
	m.Rebalancer = worker.NewRebalancer(m, &rebalancerStoreT{
		ObjectStore: m.stores,
		QuotaStore:  m.stores,
	})
	m.Replicator = worker.NewReplicator(m, &replicatorStoreT{
		ObjectStore:      m.stores,
		ReplicationStore: m.stores,
		QuotaStore:       m.stores,
	})
	m.OverReplicationCleaner = worker.NewOverReplicationCleaner(m, &overReplicationStoreT{
		ReplicationStore: m.stores,
		QuotaStore:       m.stores,
	})
	m.CleanupWorker = worker.NewCleanupWorker(m, m.stores, 10, "test-instance", 5*time.Minute)
	m.PendingReaper = worker.NewPendingReaper(m, m.stores, 0, 0, 0)
	m.Scrubber = worker.NewScrubber(m, m.stores, nil)
	m.WireDrain(drain.New(
		m,
		m.stores,
		m.stores,
		m.stores,
		m.MultipartManager.AbortMultipartUploadsOnBackend,
		m.CleanupWorker.ProcessCleanupQueue,
	))
	return m
}

// testStoresFromMock returns m typed as the wide metadata-store contract
// every consumer depends on. A no-op identity since proxy.Stores has been
// folded into core.MetadataStore; kept so existing call sites read the
// same way as the production DI wiring.
func testStoresFromMock(m core.MetadataStore) core.MetadataStore { return m }
