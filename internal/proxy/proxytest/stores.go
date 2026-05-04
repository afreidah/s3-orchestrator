// -------------------------------------------------------------------------------
// proxytest - Cross-Package Test Helpers for the Proxy Package
//
// Author: Alex Freidah
//
// Holds helpers that build proxy types from test fixtures. Lives in a
// dedicated subpackage so the production package's stores.go stays free
// of test-only API. Imported only from *_test.go files.
// -------------------------------------------------------------------------------

// Package proxytest provides cross-package test helpers for the proxy
// package. Importing it from production code is not supported.
package proxytest

import (
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// AllRoles is the structural requirement StoresFromMock imposes on its
// argument: any value that satisfies every narrow role interface.
type AllRoles interface {
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

// AttachWorkers builds every worker (rebalancer, replicator, etc.) plus
// the drain manager and attaches them to mgr. Mirrors the per-worker DI
// providers, just inline. Mock-based cross-package tests that hold a
// *proxy.BackendManager call this with the same mock they fed
// StoresFromMock so mgr.Rebalancer / mgr.Replicator / mgr.DrainManager
// are populated. Integration tests that use CB-wrapped real stores call
// AttachWorkersWithStores instead. Production code never calls either  - 
// production wiring goes through the DI providers in internal/di.
func AttachWorkers(mgr *proxy.BackendManager, m AllRoles) {
	stores := proxy.Stores{
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
	AttachWorkersWithStores(mgr, &stores)
}

// AttachWorkersWithStores is like AttachWorkers but takes a fully-built
// proxy.Stores bag so callers can pass per-role CB wrappers (the shape
// integration tests use) instead of a single mock value. Pointer receiver
// because proxy.Stores is large enough (12 interface words) that copying
// it on each call trips the gocritic hugeParam check.
func AttachWorkersWithStores(mgr *proxy.BackendManager, s *proxy.Stores) {
	mgr.Rebalancer = worker.NewRebalancer(mgr, struct {
		core.ObjectStore
		core.QuotaStore
	}{ObjectStore: s.Object, QuotaStore: s.Quota})

	mgr.Replicator = worker.NewReplicator(mgr, struct {
		core.ObjectStore
		core.ReplicationStore
		core.QuotaStore
	}{ObjectStore: s.Object, ReplicationStore: s.Replication, QuotaStore: s.Quota})

	mgr.OverReplicationCleaner = worker.NewOverReplicationCleaner(mgr, struct {
		core.ReplicationStore
		core.QuotaStore
	}{ReplicationStore: s.Replication, QuotaStore: s.Quota})

	mgr.CleanupWorker = worker.NewCleanupWorker(mgr, s.Cleanup, 10)
	if s.Pending != nil {
		mgr.PendingReaper = worker.NewPendingReaper(mgr, s.Pending, 0, 0, 0)
	}
	mgr.Scrubber = worker.NewScrubber(mgr, s.Integrity, nil)

	mgr.WireDrain(drain.New(
		mgr,
		s.Object,
		s.Quota,
		s.BackendLifecycle,
		mgr.MultipartManager.AbortMultipartUploadsOnBackend,
		mgr.CleanupWorker.ProcessCleanupQueue,
	))
}

// StoresFromMock returns a proxy.Stores bag where every field points at
// the same value. Test-only: pass a single mock that satisfies all role
// interfaces (e.g. testutil.MockStore) and receive a fully-populated
// Stores ready to drop into BackendManagerConfig.
func StoresFromMock(m AllRoles) proxy.Stores {
	return proxy.Stores{
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
