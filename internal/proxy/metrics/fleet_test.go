// -------------------------------------------------------------------------------
// Fleet Gauge Tests
//
// Author: Alex Freidah
//
// Covers reading the fleet-wide figures: every figure in one snapshot, coverage
// scoped to the backends the scrub queue draws from and aged from its head,
// coverage gauges published only while the sweep runs, and capacity warnings
// left to whichever instance holds the lock.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"slices"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate/sharedstatetest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// newShared returns shared state over an in-memory store, standing in for the
// Redis the instances share, and the store so a test can seed or fail it.
func newShared() (*sharedstate.Shared, *sharedstatetest.Memory) {
	mem := sharedstatetest.NewMemory()
	return sharedstate.New(mem, "test"), mem
}

// twoBackendLedger holds verifiable copies on b1, which the sweep reaches, and
// on b2, which it does not.
var twoBackendLedger = core.LedgerStats{
	"b1": {Objects: 5, Verifiable: 5, NeverVerified: 2},
	"b2": {Objects: 40, Verifiable: 40, NeverVerified: 40},
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestFleetSnapshot_ReadsEveryFigure verifies one snapshot carries the quota,
// ledger and multipart reads, and coverage scoped to the reachable backends:
// the never-verified copies on b1, the verifiable copies on b2 as deferred,
// and the age of the scrub queue's head, read over b1 alone.
func TestFleetSnapshot_ReadsEveryFigure(t *testing.T) {
	t.Parallel()
	scrubbed := time.Now().Add(-36 * time.Hour)
	var reached []string
	mc := testCollector(CollectorDeps{Store: &fakeStore{
		stats:     map[string]core.QuotaStat{"b1": {BytesUsed: 100}},
		ledger:    twoBackendLedger,
		multipart: map[string]int64{"b1": 3},
		head:      []core.ObjectLocation{{BackendName: "b1", LastScrubbedAt: &scrubbed}},
		reached:   &reached,
	}})

	snap, err := mc.FleetSnapshot(context.Background())
	if err != nil {
		t.Fatalf("FleetSnapshot: %v", err)
	}
	if snap.Quota["b1"].BytesUsed != 100 || snap.Ledger["b2"].Objects != 40 || snap.Multipart["b1"] != 3 {
		t.Errorf("snapshot = %+v, want every read carried through", snap)
	}
	if !slices.Equal(reached, []string{"b1"}) {
		t.Errorf("scrub head read over %v, want the reachable [b1]", reached)
	}
	if age := snap.Coverage.OldestUnverifiedAge; age < 36*time.Hour || age > 36*time.Hour+time.Minute {
		t.Errorf("oldest unverified age = %v, want about 36h", age)
	}
	if snap.Coverage.NeverVerified != 2 || snap.Coverage.Deferred != 40 {
		t.Errorf("coverage = %+v, want 2 never verified on b1 and b2's 40 deferred", snap.Coverage)
	}
}

// TestFleetSnapshot_NeverScrubbedHeadAgesFromItsWrite verifies a queue head
// never scrubbed is aged from when it was written, as the queue orders it.
func TestFleetSnapshot_NeverScrubbedHeadAgesFromItsWrite(t *testing.T) {
	t.Parallel()
	mc := testCollector(CollectorDeps{Store: &fakeStore{
		ledger: twoBackendLedger,
		head:   []core.ObjectLocation{{BackendName: "b1", CreatedAt: time.Now().Add(-2 * time.Hour)}},
	}})

	snap, err := mc.FleetSnapshot(context.Background())
	if err != nil {
		t.Fatalf("FleetSnapshot: %v", err)
	}
	if age := snap.Coverage.OldestUnverifiedAge; age < 2*time.Hour || age > 2*time.Hour+time.Minute {
		t.Errorf("oldest unverified age = %v, want about 2h", age)
	}
}

// TestRefreshGauges_CoverageOnlyWhileSweeping verifies the coverage gauges are
// left alone while no sweep runs, so a fleet nothing verifies reports no age
// that nothing would lower. Deliberately not parallel: the gauges are
// process-wide.
func TestRefreshGauges_CoverageOnlyWhileSweeping(t *testing.T) {
	store := &fakeStore{ledger: twoBackendLedger}
	telemetry.IntegrityNeverVerifiedCopies.Set(-1)

	idle := testCollector(CollectorDeps{Store: store, Scrub: scrubReach{reachable: []string{"b1"}}})
	if err := idle.refreshFleet(context.Background(), nil); err != nil {
		t.Fatalf("refreshFleet: %v", err)
	}
	if got := promtest.ToFloat64(telemetry.IntegrityNeverVerifiedCopies); got != -1 {
		t.Errorf("never-verified gauge = %v with no sweep, want it untouched", got)
	}

	sweeping := testCollector(CollectorDeps{Store: store})
	if err := sweeping.refreshFleet(context.Background(), nil); err != nil {
		t.Fatalf("refreshFleet: %v", err)
	}
	if got := promtest.ToFloat64(telemetry.IntegrityNeverVerifiedCopies); got != 2 {
		t.Errorf("never-verified gauge = %v, want b1's 2", got)
	}
	if got := promtest.ToFloat64(telemetry.IntegrityDeferredCopies); got != 40 {
		t.Errorf("deferred gauge = %v, want b2's 40", got)
	}
}

// TestWarnNearCapacity_LeftToTheLockHolder verifies an instance that finds the
// capacity lock held elsewhere raises nothing, and one that takes it warns for
// a backend past the threshold and not for one below it. Deliberately not
// parallel: the test captures the event emitter.
func TestWarnNearCapacity_LeftToTheLockHolder(t *testing.T) {
	var warned []string
	event.SetEmitter(func(ev event.Event) {
		if ev.Type == event.BackendCapacityWarning {
			warned = append(warned, ev.Subject)
		}
	})
	t.Cleanup(func() { event.SetEmitter(nil) })
	stats := map[string]core.QuotaStat{
		"full": {BytesUsed: 900, BytesLimit: 1000},
		"half": {BytesUsed: 500, BytesLimit: 1000},
		"open": {BytesUsed: 900},
	}

	testCollector(CollectorDeps{Store: &fakeStore{lockBusy: true}}).warnNearCapacity(context.Background(), stats)
	if len(warned) != 0 {
		t.Fatalf("warned for %v without the lock", warned)
	}

	testCollector(CollectorDeps{Store: &fakeStore{}}).warnNearCapacity(context.Background(), stats)
	if !slices.Equal(warned, []string{"full"}) {
		t.Errorf("warned for %v, want only the full backend", warned)
	}
}
