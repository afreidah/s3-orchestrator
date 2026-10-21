// -------------------------------------------------------------------------------
// SQLite Store Tests - Usage Batches
//
// Author: Alex Freidah
//
// Covers ApplyUsageBatch and GetUsageForPeriod: totals and pools accumulate
// additively, a batch with an ID applies at most once, old flush IDs are
// pruned, a failed batch writes nothing, and reads are scoped to one period.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// usageBackend, usagePeriod, usagePool and usageWorkload name the rows the
// usage tests write.
const (
	usageBackend  = "backend-a"
	usagePeriod   = "2026-03"
	usagePool     = "class_a"
	usageWorkload = "client"
)

// usageBatch builds a batch for usageBackend in usagePeriod with the given ID,
// API request count and pools, all charged to usageWorkload.
func usageBatch(id string, api int64, pools core.PoolUsage) *core.UsageBatch {
	return &core.UsageBatch{
		ID:      id,
		Backend: usageBackend,
		Period:  usagePeriod,
		Workloads: core.WorkloadUsage{usageWorkload: {
			UsageStat: core.UsageStat{APIRequests: api, EgressBytes: 2 * api, IngressBytes: 3 * api},
			Pools:     pools,
		}},
	}
}

// applyBatch applies b and fails the test on error.
// Shared by the tests that only care about the rows afterwards.
func applyBatch(t *testing.T, s *Store, b *core.UsageBatch) {
	t.Helper()
	if err := s.ApplyUsageBatch(context.Background(), b); err != nil {
		t.Fatalf("ApplyUsageBatch(%q): %v", b.ID, err)
	}
}

// readUsage returns usageBackend's usage for period, failing the test on a
// read error.
func readUsage(t *testing.T, s *Store, period string) core.Usage {
	t.Helper()
	usage, err := s.GetUsageForPeriod(context.Background(), period)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	return usage[usageBackend].Total()
}

// flushIDRecorded reports whether usage_flushes holds id.
// The tests use it to check what a batch recorded and what pruning removed.
func flushIDRecorded(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM usage_flushes WHERE flush_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count usage_flushes: %v", err)
	}
	return n > 0
}

// TestApplyUsageBatch_AccumulatesTotalsAndPools verifies batches without an
// ID add to the totals and pool rows, and a read joins the two per backend.
func TestApplyUsageBatch_AccumulatesTotalsAndPools(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	applyBatch(t, s, usageBatch("", 10, core.PoolUsage{usagePool: 10, "class_b": 3}))
	applyBatch(t, s, usageBatch("", 5, core.PoolUsage{usagePool: 5}))
	other := usageBatch("", 1, core.PoolUsage{usagePool: 1})
	other.Backend = "backend-b"
	applyBatch(t, s, other)

	usage, err := s.GetUsageForPeriod(context.Background(), usagePeriod)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	a := usage[usageBackend].Total()
	if a.UsageStat != (core.UsageStat{APIRequests: 15, EgressBytes: 30, IngressBytes: 45}) {
		t.Errorf("backend-a totals = %+v, want 15/30/45", a.UsageStat)
	}
	if a.Pools[usagePool] != 15 || a.Pools["class_b"] != 3 {
		t.Errorf("backend-a pools = %v, want class_a=15 class_b=3", a.Pools)
	}
	if b := usage["backend-b"].Total(); b.APIRequests != 1 || b.Pools[usagePool] != 1 {
		t.Errorf("backend-b = %+v, want 1 request and class_a=1", b)
	}
}

// TestApplyUsageBatch_KeepsWorkloadsApart verifies each workload in a batch
// gets its own totals and pool rows, read back under the same workload.
func TestApplyUsageBatch_KeepsWorkloadsApart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	b := usageBatch("", 4, core.PoolUsage{usagePool: 4})
	b.Workloads["scrub"] = core.Usage{UsageStat: core.UsageStat{EgressBytes: 900}, Pools: core.PoolUsage{usagePool: 1}}
	applyBatch(t, s, b)

	usage, err := s.GetUsageForPeriod(context.Background(), usagePeriod)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	w := usage[usageBackend]
	if w[usageWorkload].APIRequests != 4 || w[usageWorkload].Pools[usagePool] != 4 {
		t.Errorf("client = %+v, want 4 requests and class_a=4", w[usageWorkload])
	}
	if w["scrub"].EgressBytes != 900 || w["scrub"].Pools[usagePool] != 1 {
		t.Errorf("scrub = %+v, want 900 egress and class_a=1", w["scrub"])
	}
}

// TestApplyUsageBatch_AppliesIDOnce verifies a batch found again after its
// commit changes nothing, while a batch with a new ID still applies.
func TestApplyUsageBatch_AppliesIDOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	applyBatch(t, s, usageBatch("flush-1", 4, core.PoolUsage{usagePool: 4}))
	applyBatch(t, s, usageBatch("flush-1", 4, core.PoolUsage{usagePool: 4}))
	if u := readUsage(t, s, usagePeriod); u.APIRequests != 4 || u.Pools[usagePool] != 4 {
		t.Fatalf("after a repeated ID usage = %+v, want it counted once", u)
	}

	applyBatch(t, s, usageBatch("flush-2", 1, core.PoolUsage{usagePool: 1}))
	if u := readUsage(t, s, usagePeriod); u.APIRequests != 5 || u.Pools[usagePool] != 5 {
		t.Errorf("after a new ID usage = %+v, want 5 requests and class_a=5", u)
	}
	if !flushIDRecorded(t, s, "flush-1") || !flushIDRecorded(t, s, "flush-2") {
		t.Error("both applied flush IDs should be recorded")
	}
}

// TestApplyUsageBatch_SkipsZeroPools keeps a flush from writing rows for pools
// nothing charged, while the totals row is still written.
func TestApplyUsageBatch_SkipsZeroPools(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	applyBatch(t, s, usageBatch("", 2, core.PoolUsage{usagePool: 0}))

	u := readUsage(t, s, usagePeriod)
	if u.APIRequests != 2 {
		t.Errorf("api_requests = %d, want 2", u.APIRequests)
	}
	if len(u.Pools) != 0 {
		t.Errorf("pools = %v, want no row for a zero pool", u.Pools)
	}
}

// TestApplyUsageBatch_PrunesOldFlushIDs verifies applying a fresh ID drops IDs
// recorded more than 40 days ago and keeps younger ones.
func TestApplyUsageBatch_PrunesOldFlushIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const day = 24 * time.Hour
	for id, age := range map[string]time.Duration{"old": 41 * day, "recent": 39 * day} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO usage_flushes (flush_id, applied_at) VALUES (?, ?)`,
			id, formatTime(time.Now().Add(-age))); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	applyBatch(t, s, usageBatch("fresh", 1, nil))

	if flushIDRecorded(t, s, "old") {
		t.Error("an ID older than 40 days should be pruned")
	}
	if !flushIDRecorded(t, s, "recent") || !flushIDRecorded(t, s, "fresh") {
		t.Error("IDs younger than 40 days should be kept")
	}
}

// TestApplyUsageBatch_FailureWritesNothing verifies a failure at any step rolls
// back the whole batch, flush ID included, so the batch applies on a retry.
func TestApplyUsageBatch_FailureWritesNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"totals": `CREATE TRIGGER reject BEFORE INSERT ON backend_usage
			BEGIN SELECT RAISE(ABORT, 'rejected'); END`,
		"pool": `CREATE TRIGGER reject BEFORE INSERT ON backend_request_usage
			BEGIN SELECT RAISE(ABORT, 'rejected'); END`,
		"prune": `CREATE TRIGGER reject BEFORE DELETE ON usage_flushes
			BEGIN SELECT RAISE(ABORT, 'rejected'); END`,
	}
	for name, trigger := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertRejectedBatchWritesNothing(t, trigger)
		})
	}
}

// assertRejectedBatchWritesNothing installs trigger on a fresh store with a
// stale flush ID, then checks a rejected batch leaves no usage and no ID.
func assertRejectedBatchWritesNothing(t *testing.T, trigger string) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO usage_flushes (flush_id, applied_at) VALUES ('stale', ?)`,
		formatTime(time.Now().AddDate(0, 0, -50))); err != nil {
		t.Fatalf("seed stale id: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, trigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := s.ApplyUsageBatch(ctx, usageBatch("flush-x", 3, core.PoolUsage{usagePool: 3})); err == nil {
		t.Fatal("ApplyUsageBatch should fail when a write is rejected")
	}
	if u := readUsage(t, s, usagePeriod); !u.IsZero() {
		t.Errorf("usage = %+v, want nothing written by a failed batch", u)
	}
	if flushIDRecorded(t, s, "flush-x") {
		t.Error("a failed batch must not record its flush ID")
	}
}

// TestUsageQueries_SurfaceDatabaseErrors keeps a failing database from
// reading as an unused budget or a flush that succeeded.
func TestUsageQueries_SurfaceDatabaseErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.db.Close(); err != nil {
		t.Fatalf("closing the test database: %v", err)
	}

	if err := s.ApplyUsageBatch(ctx, usageBatch("", 1, nil)); err == nil {
		t.Error("ApplyUsageBatch should surface a closed database")
	}
	if _, err := s.GetUsageForPeriod(ctx, usagePeriod); err == nil {
		t.Error("GetUsageForPeriod should surface a closed database")
	}
}

// TestGetUsageForPeriod_ScopedToPeriod pins the monthly rollover: a new period
// starts empty rather than inheriting the previous month's counts.
func TestGetUsageForPeriod_ScopedToPeriod(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	applyBatch(t, s, usageBatch("", 9, core.PoolUsage{usagePool: 9}))

	usage, err := s.GetUsageForPeriod(context.Background(), "2026-04")
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	if len(usage) != 0 {
		t.Errorf("usage = %v, want the next period to start empty", usage)
	}
}
