// -------------------------------------------------------------------------------
// Worker Service Tests
//
// Author: Alex Freidah
//
// Pins what a worker's service is built from: it is named for the workload,
// ticks on the interval the config set, and runs no pass while its config
// turns the worker off.
// -------------------------------------------------------------------------------

package worker

import (
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
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

	for _, tc := range []struct {
		svc      *tickrunner.Service
		workload audit.Workload
		interval time.Duration
	}{
		{NewRebalancerService(rb, testx.Locker{}).(*tickrunner.Service), audit.WorkloadRebalance, 2 * time.Hour},
		{NewReplicatorService(rep, testx.Locker{}).(*tickrunner.Service), audit.WorkloadReplication, rep.Config().WorkerInterval},
	} {
		if tc.svc.Name() != string(tc.workload) || tc.svc.Interval() != tc.interval {
			t.Errorf("service = %q every %v, want %q every %v", tc.svc.Name(), tc.svc.Interval(), tc.workload, tc.interval)
		}
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
