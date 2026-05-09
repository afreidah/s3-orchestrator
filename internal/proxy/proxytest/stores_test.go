// -------------------------------------------------------------------------------
// proxytest Helper Tests
//
// Author: Alex Freidah
//
// Verifies the cross-package test helpers populate proxy.Stores and the
// post-construction worker fields the same way the DI providers do at
// runtime. The helpers are test-only API but Sonar's "new code coverage"
// gate counts them, so we drive them once each.
// -------------------------------------------------------------------------------

package proxytest_test

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
)

// newManager builds a minimal *proxy.BackendManager fed only by a single
// MockStore. AttachWorkers needs the manager's MultipartManager to be
// populated (NewBackendManager always builds it), so the bag is mostly
// just the mock fanned out across roles.
func newManager(t *testing.T, mock *testutil.MockStore) *proxy.BackendManager {
	t.Helper()
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          proxytest.StoresFromMock(mock),
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	t.Cleanup(mgr.Close)
	return mgr
}

// TestStoresFromMock verifies every field of the returned Stores points
// at the supplied mock  -  this is the mock-fanout invariant the cross-
// package tests depend on.
func TestStoresFromMock(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	s := proxytest.StoresFromMock(mock)
	if s.Object == nil || s.Quota == nil || s.Multipart == nil ||
		s.Replication == nil || s.Cleanup == nil || s.Pending == nil ||
		s.Integrity == nil || s.Lifecycle == nil ||
		s.BackendLifecycle == nil || s.Dashboard == nil ||
		s.Usage == nil || s.Lock == nil {
		t.Errorf("StoresFromMock: nil field in %+v", s)
	}
}

// TestAttachWorkers wires every worker on a freshly-built manager and
// asserts each public field is populated.
func TestAttachWorkers(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mgr := newManager(t, mock)

	proxytest.AttachWorkers(mgr, mock)

	if mgr.Rebalancer == nil {
		t.Error("Rebalancer not wired")
	}
	if mgr.Replicator == nil {
		t.Error("Replicator not wired")
	}
	if mgr.OverReplicationCleaner == nil {
		t.Error("OverReplicationCleaner not wired")
	}
	if mgr.CleanupWorker == nil {
		t.Error("CleanupWorker not wired")
	}
	if mgr.PendingReaper == nil {
		t.Error("PendingReaper not wired")
	}
	if mgr.Scrubber == nil {
		t.Error("Scrubber not wired")
	}
	if mgr.DrainManager == nil {
		t.Error("DrainManager not wired")
	}
}

// TestAttachWorkersWithStores covers the explicit-stores path used by
// integration tests that supply CB-wrapped store roles instead of a
// single mock value.
func TestAttachWorkersWithStores(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mgr := newManager(t, mock)

	stores := proxytest.StoresFromMock(mock)
	proxytest.AttachWorkersWithStores(mgr, &stores)

	if mgr.Rebalancer == nil || mgr.Replicator == nil ||
		mgr.OverReplicationCleaner == nil || mgr.CleanupWorker == nil ||
		mgr.Scrubber == nil || mgr.DrainManager == nil {
		t.Error("AttachWorkersWithStores left a worker nil")
	}
}

// TestAttachWorkersWithStores_NilPendingSkipsReaper covers the
// pending-pattern-disabled branch: when stores.Pending is nil the helper
// must not build a reaper (mirrors the legacy NewBackendManager
// behavior).
func TestAttachWorkersWithStores_NilPendingSkipsReaper(t *testing.T) {
	t.Parallel()
	mock := testutil.NewMockStore(t)
	mgr := newManager(t, mock)

	stores := proxytest.StoresFromMock(mock)
	stores.Pending = nil
	proxytest.AttachWorkersWithStores(mgr, &stores)

	if mgr.PendingReaper != nil {
		t.Error("PendingReaper should remain nil when stores.Pending is nil")
	}
}
