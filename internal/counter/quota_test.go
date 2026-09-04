// -------------------------------------------------------------------------------
// QuotaTracker Tests
//
// Author: Alex Freidah
//
// Tests for the in-memory byte-reservation tracker: reservation against a
// backend's ceiling, the handle's commit and release semantics, the routing
// answers placement reads, and the flush swap the usage service drains.
// -------------------------------------------------------------------------------

package counter

import (
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// trackerWith builds a tracker over the named baselines, which is the state
// production leaves it in after a flush.
func trackerWith(baselines map[string]core.BackendQuotaUsage) *QuotaTracker {
	names := make([]string, 0, len(baselines))
	for name := range baselines {
		names = append(names, name)
	}
	q := NewQuotaTracker(names)
	q.SetBaselines(baselines)
	return q
}

// -------------------------------------------------------------------------
// BASELINE
// -------------------------------------------------------------------------

// TestQuotaTracker_Baseline_ReportsWhetherItHoldsTheRow asserts a backend the
// tracker was never given is reported as absent rather than as a zero row,
// which is what keeps an unprovisioned backend out of routing.
func TestQuotaTracker_Baseline_ReportsWhetherItHoldsTheRow(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 100},
	})

	got, ok := q.Baseline("b1")
	if !ok {
		t.Fatal("b1 baseline missing")
	}
	if got.BytesUsed != 100 || got.BytesLimit != 1000 {
		t.Errorf("baseline = %+v, want limit 1000 / used 100", got)
	}
	if _, ok := q.Baseline("nope"); ok {
		t.Error("unknown backend reported a baseline")
	}
}

// TestQuotaTracker_SetBaselines_NilClearsRatherThanPanics asserts a nil
// snapshot leaves the tracker holding no rows, so a refresh that came back
// empty refuses writes instead of dereferencing nothing.
func TestQuotaTracker_SetBaselines_NilClearsRatherThanPanics(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{"b1": {BackendName: "b1"}})

	q.SetBaselines(nil)

	if _, ok := q.Baseline("b1"); ok {
		t.Error("baseline survived a nil refresh")
	}
	if got := q.Available("b1"); got != 0 {
		t.Errorf("available = %d, want 0 after the rows were cleared", got)
	}
}

// -------------------------------------------------------------------------
// RESERVATION
// -------------------------------------------------------------------------

// TestQuotaTracker_Reserve_AdmitsWhatFits asserts a write inside the headroom
// is admitted and its bytes show up in the unflushed delta.
func TestQuotaTracker_Reserve_AdmitsWhatFits(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 900},
	})

	res := q.Reserve("b1", 100)
	if res == nil {
		t.Fatal("reservation refused a write that fits exactly")
	}
	if res.Backend() != "b1" {
		t.Errorf("Backend() = %q, want b1", res.Backend())
	}
	if got := q.Delta("b1"); got != 100 {
		t.Errorf("delta = %d, want 100", got)
	}
}

// TestQuotaTracker_Reserve_RefusesWhatWouldCrossTheLimit asserts a refused
// reservation changes nothing, so the caller can try the next candidate.
func TestQuotaTracker_Reserve_RefusesWhatWouldCrossTheLimit(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 900},
	})

	if q.Reserve("b1", 101) != nil {
		t.Fatal("reservation admitted a write past the ceiling")
	}
	if got := q.Delta("b1"); got != 0 {
		t.Errorf("delta = %d, want 0 after a refusal", got)
	}
}

// TestQuotaTracker_Reserve_CountsOrphanAndInflightBytes asserts the headroom is
// judged against everything occupying the backend, not just what the ledger
// recorded. A backend holding orphans is fuller than its bytes_used says.
func TestQuotaTracker_Reserve_CountsOrphanAndInflightBytes(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 800, OrphanBytes: 100, InflightBytes: 50},
	})

	if q.Reserve("b1", 100) != nil {
		t.Fatal("orphan and in-flight bytes were not counted against the headroom")
	}
	if q.Reserve("b1", 50) == nil {
		t.Error("reservation refused a write that fits the remaining 50 bytes")
	}
}

// TestQuotaTracker_Reserve_UnlimitedBackendAlwaysAdmits asserts a zero
// bytes_limit means no enforcement, which is how the schema spells it.
func TestQuotaTracker_Reserve_UnlimitedBackendAlwaysAdmits(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesUsed: 1 << 40},
	})

	res := q.Reserve("b1", 1<<30)
	if res == nil {
		t.Fatal("unlimited backend refused a reservation")
	}
	if got := q.Delta("b1"); got != 1<<30 {
		t.Errorf("delta = %d, want the reserved bytes", got)
	}
}

// TestQuotaTracker_Reserve_UnknownBackendIsRefused asserts a backend the
// tracker holds no row for cannot be proven to have room, so it is refused
// rather than optimistically admitted.
func TestQuotaTracker_Reserve_UnknownBackendIsRefused(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker([]string{"b1"})

	if q.Reserve("b1", 1) != nil {
		t.Error("a tracker with no baselines admitted a reservation")
	}
}

// TestQuotaTracker_Reserve_ConcurrentWritesCannotBothFit asserts the
// compare-and-swap loop admits only as many concurrent writes as the headroom
// holds. Two writes that each fit on their own must not both succeed when only
// one of them fits.
func TestQuotaTracker_Reserve_ConcurrentWritesCannotBothFit(t *testing.T) {
	t.Parallel()
	const writers = 32
	// Room for exactly 10 of the 32 attempted writes.
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 100, BytesUsed: 0},
	})

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for range writers {
		wg.Go(func() {
			if q.Reserve("b1", 10) != nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if admitted != 10 {
		t.Errorf("admitted %d writes, want exactly 10", admitted)
	}
	if got := q.Delta("b1"); got != 100 {
		t.Errorf("delta = %d, want 100", got)
	}
}

// -------------------------------------------------------------------------
// RESERVATION HANDLE
// -------------------------------------------------------------------------

// TestReservation_Commit_ReplacesTheClaimWithTheCommittedBytes asserts the
// claim is dropped in the same breath as the deltas land, so the write's bytes
// are counted once rather than twice.
func TestReservation_Commit_ReplacesTheClaimWithTheCommittedBytes(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000},
		"b2": {BackendName: "b2", BytesLimit: 1000},
	})

	res := q.Reserve("b1", 100)
	// The ledger recorded the write on b1 and displaced an older copy on b2.
	res.Commit(core.QuotaDeltas{"b1": 100, "b2": -40})

	if got := q.Delta("b1"); got != 100 {
		t.Errorf("b1 delta = %d, want 100 (committed bytes, claim dropped)", got)
	}
	if got := q.Delta("b2"); got != -40 {
		t.Errorf("b2 delta = %d, want -40", got)
	}
}

// TestReservation_Release_ReturnsTheClaim asserts a write that did not land
// gives its bytes back, since a leaked reservation is flushed into bytes_used
// as though bytes had been stored.
func TestReservation_Release_ReturnsTheClaim(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000},
	})

	q.Reserve("b1", 100).Release()

	if got := q.Delta("b1"); got != 0 {
		t.Errorf("delta = %d, want 0 after the claim was returned", got)
	}
}

// TestReservation_DisposedOnlyOnce asserts a Release deferred at the point of
// reservation is a no-op once the write committed, which is what lets every
// error path share one deferred call.
func TestReservation_DisposedOnlyOnce(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000},
	})

	res := q.Reserve("b1", 100)
	res.Commit(core.QuotaDeltas{"b1": 100})
	res.Release()
	res.Release()

	if got := q.Delta("b1"); got != 100 {
		t.Errorf("delta = %d, want 100; a disposed reservation was refunded again", got)
	}
}

// TestReservation_NilHandleIsSafe asserts the refused reservation accepts both
// disposal methods, so a caller can defer Release on the result of Reserve
// before deciding whether it got anything.
func TestReservation_NilHandleIsSafe(t *testing.T) {
	t.Parallel()
	var res *Reservation

	res.Commit(core.QuotaDeltas{"b1": 1})
	res.Release()

	if got := res.Backend(); got != "" {
		t.Errorf("Backend() = %q, want empty for a refused reservation", got)
	}
}

// -------------------------------------------------------------------------
// RECORDING
// -------------------------------------------------------------------------

// TestQuotaTracker_Apply_FoldsEveryBackendsDelta asserts a committed
// transaction's deltas all land, for the mutations that never reserved.
func TestQuotaTracker_Apply_FoldsEveryBackendsDelta(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker([]string{"b1", "b2"})

	q.Apply(core.QuotaDeltas{"b1": -100, "b2": 250})

	if got := q.Delta("b1"); got != -100 {
		t.Errorf("b1 delta = %d, want -100", got)
	}
	if got := q.Delta("b2"); got != 250 {
		t.Errorf("b2 delta = %d, want 250", got)
	}
}

// TestQuotaTracker_Record_IgnoresAZeroDelta asserts a no-op mutation does not
// create a delta entry, so a quiet backend costs no UPDATE at flush time.
func TestQuotaTracker_Record_IgnoresAZeroDelta(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker(nil)

	q.Record("b1", 0)

	if got := q.SwapDeltas(); len(got) != 0 {
		t.Errorf("deltas = %v, want none", got)
	}
}

// TestQuotaTracker_Record_AccumulatesForAnUnnamedBackend asserts a backend the
// tracker was not constructed with still accumulates, since a delta arrives
// from a mutation whether or not the backend was named up front.
func TestQuotaTracker_Record_AccumulatesForAnUnnamedBackend(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker(nil)

	q.Record("late", -50)
	q.Record("late", -25)

	if got := q.Delta("late"); got != -75 {
		t.Errorf("delta = %d, want -75", got)
	}
	if got := q.Delta("never-charged"); got != 0 {
		t.Errorf("delta = %d, want 0 for a backend nothing accumulated under", got)
	}
}

// TestQuotaDeltas_Add_LeavesANilMapAlone asserts a path that never allocated a
// map is not forced to, which is what lets a caller pass nil through.
func TestQuotaDeltas_Add_LeavesANilMapAlone(t *testing.T) {
	t.Parallel()
	var deltas core.QuotaDeltas

	deltas.Add("b1", 100)

	if deltas != nil {
		t.Errorf("deltas = %v, want nil", deltas)
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// TestQuotaTracker_Available_SubtractsTheUnflushedDelta asserts routing sees
// the writes this instance admitted since the last flush, which the row does
// not yet know about.
func TestQuotaTracker_Available_SubtractsTheUnflushedDelta(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 400, OrphanBytes: 100},
	})

	if got := q.Available("b1"); got != 500 {
		t.Fatalf("available = %d, want 500", got)
	}
	q.Reserve("b1", 200)
	if got := q.Available("b1"); got != 300 {
		t.Errorf("available = %d, want 300 once 200 bytes were claimed", got)
	}
}

// TestQuotaTracker_Available_ClampsAtZeroWhenOverCommitted asserts an
// over-limit backend reports no room rather than a negative figure that would
// sort ahead of an empty one.
func TestQuotaTracker_Available_ClampsAtZeroWhenOverCommitted(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 100, BytesUsed: 500},
	})

	if got := q.Available("b1"); got != 0 {
		t.Errorf("available = %d, want 0", got)
	}
}

// TestQuotaTracker_Available_UnlimitedAndUnknownBackends asserts the two ends
// of the range: an unlimited backend sorts as the roomiest candidate, and one
// with no row drops out of routing entirely.
func TestQuotaTracker_Available_UnlimitedAndUnknownBackends(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"unlimited": {BackendName: "unlimited"},
	})

	if got := q.Available("unlimited"); got != math.MaxInt64 {
		t.Errorf("available = %d, want MaxInt64", got)
	}
	if got := q.Available("unknown"); got != 0 {
		t.Errorf("available = %d, want 0 for a backend with no row", got)
	}
}

// TestQuotaTracker_Utilization_ReportsTheFractionSpokenFor asserts utilization
// covers baseline plus unflushed delta, and that an unlimited or unknown
// backend reports zero so it ranks ahead of any bounded one.
func TestQuotaTracker_Utilization_ReportsTheFractionSpokenFor(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"half":      {BackendName: "half", BytesLimit: 1000, BytesUsed: 500},
		"unlimited": {BackendName: "unlimited", BytesUsed: 1 << 40},
	})

	if got := q.Utilization("half"); got != 0.5 {
		t.Errorf("utilization = %v, want 0.5", got)
	}
	q.Reserve("half", 250)
	if got := q.Utilization("half"); got != 0.75 {
		t.Errorf("utilization = %v, want 0.75 once 250 bytes were claimed", got)
	}
	if got := q.Utilization("unlimited"); got != 0 {
		t.Errorf("unlimited utilization = %v, want 0", got)
	}
	if got := q.Utilization("unknown"); got != 0 {
		t.Errorf("unknown utilization = %v, want 0", got)
	}
}

// TestQuotaTracker_RankByUtilization_OrdersEmptiestFirst asserts the single
// ordering every load-spreading placement reads from, and that it leaves the
// caller's slice alone.
func TestQuotaTracker_RankByUtilization_OrdersEmptiestFirst(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"full":   {BackendName: "full", BytesLimit: 1000, BytesUsed: 900},
		"middle": {BackendName: "middle", BytesLimit: 1000, BytesUsed: 500},
		"empty":  {BackendName: "empty", BytesLimit: 1000, BytesUsed: 10},
	})

	candidates := []string{"full", "middle", "empty"}
	ranked := q.RankByUtilization(candidates)

	if want := []string{"empty", "middle", "full"}; !slices.Equal(ranked, want) {
		t.Errorf("ranked = %v, want %v", ranked, want)
	}
	if want := []string{"full", "middle", "empty"}; !slices.Equal(candidates, want) {
		t.Errorf("caller's slice was reordered: %v", candidates)
	}
}

// -------------------------------------------------------------------------
// FLUSH
// -------------------------------------------------------------------------

// TestQuotaTracker_SwapDeltas_DrainsAndResets asserts the flush reads every
// backend's total in one swap and leaves the counter empty, so bytes are not
// written twice.
func TestQuotaTracker_SwapDeltas_DrainsAndResets(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker([]string{"b1", "b2", "quiet"})
	q.Record("b1", 500)
	q.Record("b2", -120)

	got := q.SwapDeltas()

	if len(got) != 2 || got["b1"] != 500 || got["b2"] != -120 {
		t.Errorf("deltas = %v, want b1:500 b2:-120 and no quiet backend", got)
	}
	if after := q.SwapDeltas(); len(after) != 0 {
		t.Errorf("deltas = %v, want none after the swap drained them", after)
	}
}

// TestQuotaTracker_RestoreDeltas_CarriesAFailedFlushForward asserts a flush
// that could not write gives its bytes back, rather than silently losing an
// interval of writes from bytes_used.
func TestQuotaTracker_RestoreDeltas_CarriesAFailedFlushForward(t *testing.T) {
	t.Parallel()
	q := NewQuotaTracker([]string{"b1"})
	q.Record("b1", 500)

	drained := q.SwapDeltas()
	q.Record("b1", 25) // a write admitted while the flush was in flight
	q.RestoreDeltas(drained)

	if got := q.Delta("b1"); got != 525 {
		t.Errorf("delta = %d, want 525 (restored 500 plus the 25 admitted since)", got)
	}
}
