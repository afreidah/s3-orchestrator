// -------------------------------------------------------------------------------
// QuotaTracker Tests
//
// Author: Alex Freidah
//
// Tests for the routing view of what each backend holds: the snapshot it is
// primed with, and the ordering placement reads from it.
//
// Whether a write fits is not tested here, because the tracker does not decide
// it. That lives with the statements that claim the space - the conditional
// inserts of a pending intent and of a replica - and is tested against a real
// database, where the concurrency it guards against actually exists.
// -------------------------------------------------------------------------------

package counter

import (
	"math"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// trackerWith builds a tracker over the named baselines, which is the state
// production leaves it in after a refresh.
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
// snapshot has never seen is distinguishable from one recorded as empty, so a
// caller can tell "no room" from "nothing known".
func TestQuotaTracker_Baseline_ReportsWhetherItHoldsTheRow(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000},
	})

	if _, ok := q.Baseline("nope"); ok {
		t.Error("a backend the snapshot never held reported as present")
	}
	if base, ok := q.Baseline("b1"); !ok || base.BytesLimit != 1000 {
		t.Errorf("baseline = %+v (ok=%v), want the row it was primed with", base, ok)
	}
}

// TestQuotaTracker_SetBaselines_NilClearsRatherThanPanics asserts a refresh
// that read no rows leaves an empty snapshot rather than a nil map every later
// read would fault on.
func TestQuotaTracker_SetBaselines_NilClearsRatherThanPanics(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000},
	})

	q.SetBaselines(nil)

	if _, ok := q.Baseline("b1"); ok {
		t.Error("baseline survived being cleared")
	}
	if got := q.Available("b1"); got != 0 {
		t.Errorf("available = %d, want 0 against a cleared snapshot", got)
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// TestQuotaTracker_Available_ReportsTheSnapshotHeadroom asserts the figure
// placement ranks on covers everything occupying the backend, not just the
// bytes stored: orphans awaiting cleanup and writes in flight are room a
// candidate does not have.
func TestQuotaTracker_Available_ReportsTheSnapshotHeadroom(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 400, OrphanBytes: 100, InflightBytes: 50},
	})

	if got := q.Available("b1"); got != 450 {
		t.Errorf("available = %d, want 450 (1000 - 400 - 100 - 50)", got)
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
// covers everything occupying the backend, and that an unlimited or unknown
// backend reports zero so it ranks ahead of any bounded one.
func TestQuotaTracker_Utilization_ReportsTheFractionSpokenFor(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"half":      {BackendName: "half", BytesLimit: 1000, BytesUsed: 400, OrphanBytes: 100},
		"unlimited": {BackendName: "unlimited", BytesUsed: 1 << 40},
	})

	if got := q.Utilization("half"); got != 0.5 {
		t.Errorf("utilization = %v, want 0.5", got)
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

// TestQuotaTracker_RankByUtilization_FollowsWhatThisInstancePlaced asserts the
// ranking moves as writes land rather than staying fixed until the next
// reload.
//
// Without this the snapshot is the only input, every write in the interval
// ranks the candidates identically, and spread routing degenerates into pack
// until the reload catches up.
func TestQuotaTracker_RankByUtilization_FollowsWhatThisInstancePlaced(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"a": {BackendName: "a", BytesLimit: 1000},
		"b": {BackendName: "b", BytesLimit: 1000},
	})

	first := q.RankByUtilization([]string{"a", "b"})[0]
	q.NotePlacement(first, 600)

	if got := q.RankByUtilization([]string{"a", "b"})[0]; got == first {
		t.Errorf("ranked %q first again after placing 600 bytes on it", got)
	}
}

// TestQuotaTracker_SetBaselines_DropsWhatWasPlaced asserts the correction is
// spent once the rows it was correcting have been re-read, or the same bytes
// would be counted twice.
func TestQuotaTracker_SetBaselines_DropsWhatWasPlaced(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"a": {BackendName: "a", BytesLimit: 1000},
	})
	q.NotePlacement("a", 400)
	if got := q.Available("a"); got != 600 {
		t.Fatalf("available = %d, want 600 with the placement counted", got)
	}

	// The reload reports the bytes as stored, so the correction must go.
	q.SetBaselines(map[string]core.BackendQuotaUsage{
		"a": {BackendName: "a", BytesLimit: 1000, BytesUsed: 400},
	})

	if got := q.Available("a"); got != 600 {
		t.Errorf("available = %d, want 600; the placement was counted on top of the rows", got)
	}
}

// TestQuotaTracker_RankByUtilization_RefreshChangesTheOrder asserts the ranking
// follows the snapshot rather than any state of its own, which is the whole of
// what reloading it is for.
func TestQuotaTracker_RankByUtilization_RefreshChangesTheOrder(t *testing.T) {
	t.Parallel()
	q := trackerWith(map[string]core.BackendQuotaUsage{
		"a": {BackendName: "a", BytesLimit: 1000, BytesUsed: 100},
		"b": {BackendName: "b", BytesLimit: 1000, BytesUsed: 900},
	})

	if got := q.RankByUtilization([]string{"a", "b"}); got[0] != "a" {
		t.Fatalf("ranked %v, want a first while it is the emptier", got)
	}

	q.SetBaselines(map[string]core.BackendQuotaUsage{
		"a": {BackendName: "a", BytesLimit: 1000, BytesUsed: 900},
		"b": {BackendName: "b", BytesLimit: 1000, BytesUsed: 100},
	})

	if got := q.RankByUtilization([]string{"a", "b"}); got[0] != "b" {
		t.Errorf("ranked %v, want b first once the refresh reversed them", got)
	}
}

// -------------------------------------------------------------------------
// DELTAS
// -------------------------------------------------------------------------

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
