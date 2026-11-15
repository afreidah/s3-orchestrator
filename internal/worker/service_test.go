// -------------------------------------------------------------------------------
// Worker Service Tests
//
// Author: Alex Freidah
//
// Pins what a worker's service is built from: it is named for the workload,
// ticks on the interval the config set, runs no pass while its config turns
// the worker off, and the replication service works one batch of each side of
// the queue per tick.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// TestServices_NamedForWorkloadOnTheConfiguredInterval verifies each pass
// service carries its workload's name and the interval its config set.
func TestServices_NamedForWorkloadOnTheConfiguredInterval(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops, pl, store := newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{}
	rb := NewRebalancer(RebalancerDeps{Fleet: fleetOf(ops, pl), Store: store,
		Config: syncutil.Fixed(&config.RebalanceConfig{Interval: 2 * time.Hour})})
	rep := newTestReplicator(fleetOf(ops, pl), store)
	cleaner := NewOverReplicationCleaner(OverReplicationCleanerDeps{Fleet: fleetOf(ops, pl), Store: store,
		Queue: NewReplicationQueue(store), Config: defaultReplication})

	for _, tc := range []struct {
		svc      *tickrunner.Service
		workload audit.Workload
		interval time.Duration
	}{
		{NewRebalancerService(rb, testx.Locker{}).(*tickrunner.Service), audit.WorkloadRebalance, 2 * time.Hour},
		{NewReplicationService(rep, cleaner, testx.Locker{}).(*tickrunner.Service), audit.WorkloadReplication, rep.Config().WorkerInterval},
	} {
		if tc.svc.Name() != string(tc.workload) || tc.svc.Interval() != tc.interval {
			t.Errorf("service = %q every %v, want %q every %v", tc.svc.Name(), tc.svc.Interval(), tc.workload, tc.interval)
		}
	}
}

// TestReplicationService_TickWorksOneBatchOfEachSide verifies a tick reads one
// batch of the queue's short side and one of its over side, even when the
// batch it read held keys, and that a failed short side does not keep the over
// side from running.
func TestReplicationService_TickWorksOneBatchOfEachSide(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	reads := map[core.ReplicationDirection]int{}
	store.EXPECT().ListReplicationQueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, side core.ReplicationDirection, _ int, _ []string) ([]core.QueuedReplication, error) {
			reads[side]++
			if side == core.ReplicationShort {
				return nil, errors.New("db down")
			}
			return []core.QueuedReplication{{Key: "at-factor", Direction: side, Live: 2, Held: 2}}, nil
		}).AnyTimes()
	storetest.Permissive(store)

	_, f := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{})
	cfg := syncutil.Fixed(&config.ReplicationConfig{Factor: 2, BatchSize: 10})
	queue := NewReplicationQueue(store)
	rep := NewReplicator(&ReplicatorDeps{Fleet: f, Store: store, Queue: queue, Config: cfg, Integrity: defaultIntegrity})
	cleaner := NewOverReplicationCleaner(OverReplicationCleanerDeps{Fleet: f, Store: store, Queue: queue, Config: cfg})

	NewReplicationService(rep, cleaner, testx.Locker{}).(*tickrunner.Service).Tick(t.Context())
	if reads[core.ReplicationShort] != 1 || reads[core.ReplicationOver] != 1 {
		t.Errorf("reads = %v, want one batch of each side", reads)
	}
}

// TestServices_DisabledWorkerRunsNoPass verifies a tick of a worker its config
// turns off runs no pass: the mocks fail the test on any call.
func TestServices_DisabledWorkerRunsNoPass(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	rb := NewRebalancer(RebalancerDeps{Fleet: fleetOf(NewMockOps(ctrl), NewMockPlacement(ctrl)), Store: &mockMetadataStore{},
		Config: syncutil.Fixed(&config.RebalanceConfig{Enabled: false, Interval: time.Hour})})
	NewRebalancerService(rb, testx.Locker{}).(*tickrunner.Service).Tick(t.Context())
}
