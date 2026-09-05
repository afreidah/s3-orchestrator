// -------------------------------------------------------------------------------
// Worker Test Fleet
//
// Author: Alex Freidah
//
// Builds the two collaborators every worker takes - an Ops (the backend
// runtime) and a Placement (the write coordinator) - directly from their own
// constructors. Workers depend on those two interfaces and nothing else, so a
// worker test needs no proxy stack and none of the composition around it.
// -------------------------------------------------------------------------------

package worker

import (
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// fleetTimeout bounds a backend call in the test fleet. Long enough that no
// test trips it incidentally; tests that want a timeout set their own.
const fleetTimeout = 30 * time.Second

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// fleetOpts tunes the test fleet beyond the defaults. The zero value gives
// pack routing, an unlimited local usage counter, and no drain.
// Order defaults to the backend map's keys in unspecified order, which is fine
// for a test that names one backend.
type fleetOpts struct {
	Order          []string                    // fixes the fleet order
	Routing        config.RoutingStrategy      // defaults to pack
	UsageLimits    map[string]core.UsageLimits // per-backend API and bandwidth caps
	MaxObjectSizes map[string]int64            // per-backend object size cap, for eligibility tests
	Draining       []string                    // backends the runtime should report as draining

	QuotaBaselines map[string]core.BackendQuotaUsage // seeds the byte counter; nil leaves every backend unlimited, non-nil is the whole world
}

// drainingSet reports a fixed set of backends as draining, standing in for
// drain.Manager's one-method infra.DrainChecker surface.
type drainingSet map[string]bool

// IsDraining implements infra.DrainChecker.
func (d drainingSet) IsDraining(name string) bool { return d[name] }

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newFleet builds the runtime and coordinator a worker is constructed from.
// The store is the wide metadata store; each worker constructor narrows it to
// its own role interface.
func newFleet(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) (*infra.BackendRuntime, *writepath.Coordinator) {
	t.Helper()
	if opts == nil {
		opts = &fleetOpts{}
	}

	names := opts.Order
	if names == nil {
		for name := range backends {
			names = append(names, name)
		}
	}
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend(names), opts.UsageLimits)

	routing := opts.Routing
	if routing == "" {
		routing = config.RoutingPack
	}
	rt := infra.New(&infra.Config{
		Backends:        backends,
		Order:           names,
		BackendTimeout:  fleetTimeout,
		Usage:           usage,
		Quota:           newFleetQuota(names, opts.QuotaBaselines),
		RoutingStrategy: routing,
		MaxObjectSizes:  opts.MaxObjectSizes,
	})
	rt.SetMetricsCollector(metrics.New(metrics.CollectorDeps{
		Store: store, Usage: usage, BackendNames: names,
	}))
	if len(opts.Draining) > 0 {
		d := make(drainingSet, len(opts.Draining))
		for _, name := range opts.Draining {
			d[name] = true
		}
		rt.SetDrainChecker(d)
	}
	return rt, writepath.New(rt, store)
}

// newFleetQuota builds the byte-reservation tracker with baselines already
// primed, which production does from backend_quotas before the listener opens.
//
// A caller that states no baselines gets every backend unlimited, so a test
// that is not about quota never has one refuse a write. A caller that states
// any gets exactly those: a backend left out is one the ledger has no quota row
// for, which the tracker treats as having no room rather than infinite room.
func newFleetQuota(names []string, baselines map[string]core.BackendQuotaUsage) *counter.QuotaTracker {
	primed := baselines
	if primed == nil {
		primed = make(map[string]core.BackendQuotaUsage, len(names))
		for _, name := range names {
			primed[name] = core.BackendQuotaUsage{BackendName: name}
		}
	}
	quota := counter.NewQuotaTracker(names)
	quota.SetBaselines(primed)
	return quota
}

// newPermissiveStore returns a union store mock that answers every read with
// an empty result, so a worker test states only the queries it asserts on.
func newPermissiveStore(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}

// newCleanupFor builds a CleanupWorker over a fleet of the named backends,
// returning it with the runtime so a test can read the usage counters it
// charged.
func newCleanupFor(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) (*CleanupWorker, *infra.BackendRuntime) {
	t.Helper()
	rt, _ := newFleet(t, store, backends, opts)
	return NewCleanupWorker(CleanupWorkerDeps{
		Ops: rt, Store: store, Concurrency: 10,
		InstanceID: "test-instance", ClaimGracePeriod: 5 * time.Minute,
	}), rt
}

// newReplicatorFor builds a Replicator over a fleet of the named backends.
func newReplicatorFor(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) *Replicator {
	t.Helper()
	rt, coord := newFleet(t, store, backends, opts)
	return newTestReplicator(rt, coord, store)
}

// newRebalancerFor builds a Rebalancer over a fleet of the named backends.
func newRebalancerFor(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) *Rebalancer {
	t.Helper()
	rt, coord := newFleet(t, store, backends, opts)
	return NewRebalancer(rt, coord, store)
}

// newOverRepFor builds an OverReplicationCleaner over a fleet of the named
// backends.
func newOverRepFor(
	t *testing.T, store storetest.MetadataStore, backends map[string]backend.ObjectBackend, opts *fleetOpts,
) *OverReplicationCleaner {
	t.Helper()
	rt, coord := newFleet(t, store, backends, opts)
	return NewOverReplicationCleaner(rt, coord, store)
}
