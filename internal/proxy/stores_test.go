// -------------------------------------------------------------------------------
// In-Package Test Helpers
//
// Author: Alex Freidah
//
// Mirror of testStoresFromMock for tests inside the proxy package.
// proxy's own tests can't import proxy/proxytest without an import cycle,
// so this file declares a local copy guarded by the _test.go build tag.
// External callers must use testStoresFromMock instead.
// -------------------------------------------------------------------------------

package proxy

import (
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// rebalancerStoreT/replicatorStoreT/overReplicationStoreT mirror the
// adapter types the DI package builds for production (#676 B). Declared
// in this _test.go file so proxy's own fixtures can wire workers without
// importing di. Not intended for production use.
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
// "return NewBackendManager(...)" sites in test fixtures. Mirrors the
// per-worker DI providers, just inline. Tests that reach mgr.Rebalancer
// / mgr.Replicator / etc. need the wiring this helper installs.
func wireWorkersForTest(m *BackendManager) *BackendManager {
	m.Rebalancer = worker.NewRebalancer(m, &rebalancerStoreT{
		ObjectStore: m.stores.Object,
		QuotaStore:  m.stores.Quota,
	})
	m.Replicator = worker.NewReplicator(m, &replicatorStoreT{
		ObjectStore:      m.stores.Object,
		ReplicationStore: m.stores.Replication,
		QuotaStore:       m.stores.Quota,
	})
	m.OverReplicationCleaner = worker.NewOverReplicationCleaner(m, &overReplicationStoreT{
		ReplicationStore: m.stores.Replication,
		QuotaStore:       m.stores.Quota,
	})
	m.CleanupWorker = worker.NewCleanupWorker(m, m.stores.Cleanup, 10)
	if m.stores.Pending != nil {
		m.PendingReaper = worker.NewPendingReaper(m, m.stores.Pending, 0, 0, 0)
	}
	m.Scrubber = worker.NewScrubber(m, m.stores.Integrity, nil)
	m.WireDrain(drain.New(
		m,
		m.stores.Object,
		m.stores.Quota,
		m.stores.BackendLifecycle,
		m.MultipartManager.AbortMultipartUploadsOnBackend,
		m.CleanupWorker.ProcessCleanupQueue,
	))
	return m
}

// allRoles is the structural shape testStoresFromMock requires of its
// argument — any value that satisfies every narrow role interface.
type allRoles interface {
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
}

// testStoresFromMock returns a Stores bag where every field points at the
// same mock value. Used by tests inside the proxy package; tests in other
// packages use testStoresFromMock instead.
func testStoresFromMock(m allRoles) Stores {
	return Stores{
		Object:           m,
		Quota:            m,
		Multipart:        m,
		Replication:      m,
		Cleanup:          m,
		Pending:          m,
		Integrity:        m,
		Lifecycle:        m,
		BackendLifecycle: m,
		Dashboard:        m,
		Usage:            m,
		Lock:             m,
	}
}
