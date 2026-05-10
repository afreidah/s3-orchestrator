// Helper tests for proxytest.AttachWorkers. The helper is test-only API
// but Sonar's "new code coverage" gate counts it, so we drive it once.
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
// populated (NewBackendManager always builds it).
func newManager(t *testing.T, mock *testutil.MockStore) *proxy.BackendManager {
	t.Helper()
	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]backend.ObjectBackend{},
		Stores:          mock,
		Dashboard:       mock,
		Metrics:         mock,
		Order:           []string{},
		RoutingStrategy: config.RoutingPack,
	})
	t.Cleanup(mgr.Close)
	return mgr
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
