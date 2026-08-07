// Helper tests for proxytest.BuildWorkers. The helper is test-only API
// but Sonar's "new code coverage" gate counts it, so we drive it once.
package proxytest_test

import (
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newManager builds a minimal *proxy.BackendManager fed only by a single
// MockStore. BuildWorkers needs the manager's MultipartManager to be
// populated (NewBackendManager always builds it).
func newManager(t *testing.T, mock *storetest.MockMetadataStore) *proxy.BackendManager {
	t.Helper()
	mgr := proxytest.NewManager(t, mock, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]backend.ObjectBackend{},
			Order:    []string{},
		},
		Policies: proxy.PolicyConfig{
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mock,
		},
	})
	t.Cleanup(mgr.Close)
	return mgr
}

// TestBuildWorkers wires every worker on a freshly-built manager and
// asserts each handle on the returned Workers struct is populated, plus
// that drain.Manager is installed on the manager.
func TestBuildWorkers(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	mgr := newManager(t, mock)

	w := proxytest.BuildWorkers(mgr, mock)

	if w.Rebalancer == nil {
		t.Error("Rebalancer not wired")
	}
	if w.Replicator == nil {
		t.Error("Replicator not wired")
	}
	if w.OverReplicationCleaner == nil {
		t.Error("OverReplicationCleaner not wired")
	}
	if w.CleanupWorker == nil {
		t.Error("CleanupWorker not wired")
	}
	if w.PendingReaper == nil {
		t.Error("PendingReaper not wired")
	}
	if w.Scrubber == nil {
		t.Error("Scrubber not wired")
	}
	if w.Drain == nil {
		t.Error("Drain not wired")
	}
	if mgr.Drain() == nil {
		t.Error("drain manager not injected into BackendManager")
	}
}
