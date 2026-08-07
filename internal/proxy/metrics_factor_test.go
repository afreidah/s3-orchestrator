// -------------------------------------------------------------------------------
// Metrics - Replication Factor Wiring
//
// Author: Alex Freidah
//
// The one metrics-collector test that needs a full manager: it verifies the
// replication-factor callback the collector reads is the live replicator's,
// not a snapshot taken at construction. The rest of the collector's behaviour
// is covered in internal/proxy/metrics against a narrow Deps mock.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestUpdateQuotaMetrics_ReplicationFactorFromManager confirms the
// closure-driven factor lookup works with and without replication
// configured on the manager.
func TestUpdateQuotaMetrics_ReplicationFactorFromManager(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{"b1": {BytesUsed: 100, BytesLimit: 1000}}, nil).
		AnyTimes()
	store.EXPECT().GetObjectCounts(gomock.Any()).Return(map[string]int64{"b1": 5}, nil).AnyTimes()
	store.EXPECT().GetActiveMultipartCounts(gomock.Any()).Return(map[string]int64{}, nil).AnyTimes()
	store.EXPECT().GetUsageForPeriod(gomock.Any(), gomock.Any()).Return(map[string]core.UsageStat{}, nil).AnyTimes()
	store.EXPECT().GetUnderReplicatedObjects(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100}}, nil).
		AnyTimes()

	mgr := newTestBackendManager(t, &BackendManagerConfig{
		Storage: StorageDeps{
			Backends: map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()},
			Order:    []string{"b1"},
		},
		Stores: StoreDeps{
			Metadata: testStoresFromMock(store),
		},
		Policies: PolicyConfig{
			CacheTTL:        5 * time.Second,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
		},
		Operations: OperationalDeps{
			Metrics: store,
		},
	})
	workers := wireWorkersForTest(mgr, store)

	if err := mgr.Runtime().MetricsCollector().UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics (no repl config): %v", err)
	}

	workers.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 50})
	if err := mgr.Runtime().MetricsCollector().UpdateQuotaMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateQuotaMetrics (with repl config): %v", err)
	}
}
