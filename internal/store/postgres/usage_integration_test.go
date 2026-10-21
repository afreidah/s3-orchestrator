// -------------------------------------------------------------------------------
// Postgres Store - Usage Batch Integration Tests
//
// Author: Alex Freidah
//
// Covers ApplyUsageBatch and GetUsageForPeriod against a real Postgres: totals
// and pools accumulate additively, a batch with an ID applies at most once, old
// flush IDs are pruned, a failed batch writes nothing, and reads are scoped to
// one period. Each test writes under its own backend name because the fixture
// database is shared by the whole suite.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// pgUsagePeriod and pgUsagePool name the period and pool the usage tests write.
const (
	pgUsagePeriod = "2026-05"
	pgUsagePool   = "class_a"
)

// pgUsageBatch builds a batch for backend in pgUsagePeriod with the given ID,
// API request count and pools, all charged to the client workload.
func pgUsageBatch(backend, id string, api int64, pools core.PoolUsage) *core.UsageBatch {
	return &core.UsageBatch{
		ID:      id,
		Backend: backend,
		Period:  pgUsagePeriod,
		Workloads: core.WorkloadUsage{"client": {
			UsageStat: core.UsageStat{APIRequests: api, EgressBytes: 2 * api, IngressBytes: 3 * api},
			Pools:     pools,
		}},
	}
}

// pgApplyBatch applies b and fails the test on error.
// Shared by the tests that only care about the rows afterwards.
func pgApplyBatch(t *testing.T, s *Store, b *core.UsageBatch) {
	t.Helper()
	if err := s.ApplyUsageBatch(context.Background(), b); err != nil {
		t.Fatalf("ApplyUsageBatch(%q): %v", b.ID, err)
	}
}

// pgReadUsage returns backend's usage for period, failing the test on a read
// error.
func pgReadUsage(t *testing.T, s *Store, backend, period string) core.Usage {
	t.Helper()
	usage, err := s.GetUsageForPeriod(context.Background(), period)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	return usage[backend].Total()
}

// pgFlushIDRecorded reports whether usage_flushes holds id.
// The tests use it to check what a batch recorded and what pruning removed.
func pgFlushIDRecorded(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_flushes WHERE flush_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count usage_flushes: %v", err)
	}
	return n > 0
}

// pgUsageBackend registers a backend named for the test and returns its name.
// The usage tables reference backend_quotas, so a batch needs a row there.
func pgUsageBackend(t *testing.T, s *Store) string {
	t.Helper()
	name := uniqueKey(t, "backend")
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO backend_quotas (backend_name, bytes_limit) VALUES ($1, 0) ON CONFLICT DO NOTHING`, name); err != nil {
		t.Fatalf("register backend %s: %v", name, err)
	}
	return name
}

// pgSeedFlushID records id as applied the given number of days ago.
// Pruning is judged by applied_at, so the tests backdate it directly.
func pgSeedFlushID(t *testing.T, s *Store, id string, daysAgo int) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO usage_flushes (flush_id, applied_at) VALUES ($1, NOW() - make_interval(days => $2))`,
		id, daysAgo); err != nil {
		t.Fatalf("seed flush id %s: %v", id, err)
	}
}

// TestStoreInt_ApplyUsageBatch_AccumulatesTotalsAndPools verifies batches
// without an ID add to the totals and pool rows, zero pools write no row, and
// a read joins totals and pools per backend.
func TestStoreInt_ApplyUsageBatch_AccumulatesTotalsAndPools(t *testing.T) {
	s := adapterPgStore(t)
	backend := pgUsageBackend(t, s)

	pgApplyBatch(t, s, pgUsageBatch(backend, "", 10, core.PoolUsage{pgUsagePool: 10, "class_b": 4}))
	pgApplyBatch(t, s, pgUsageBatch(backend, "", 5, core.PoolUsage{pgUsagePool: 5, "class_c": 0}))

	u := pgReadUsage(t, s, backend, pgUsagePeriod)
	if u.UsageStat != (core.UsageStat{APIRequests: 15, EgressBytes: 30, IngressBytes: 45}) {
		t.Errorf("totals = %+v, want 15/30/45", u.UsageStat)
	}
	if u.Pools[pgUsagePool] != 15 || u.Pools["class_b"] != 4 {
		t.Errorf("pools = %v, want class_a=15 class_b=4", u.Pools)
	}
	if _, charged := u.Pools["class_c"]; charged {
		t.Errorf("class_c has a row: %v; a zero pool must write nothing", u.Pools)
	}
}

// TestStoreInt_ApplyUsageBatch_AppliesIDOnce verifies a batch found again
// after its commit changes nothing, while a new ID still applies.
func TestStoreInt_ApplyUsageBatch_AppliesIDOnce(t *testing.T) {
	s := adapterPgStore(t)
	backend := pgUsageBackend(t, s)
	first, second := uniqueKey(t, "flush-1"), uniqueKey(t, "flush-2")

	pgApplyBatch(t, s, pgUsageBatch(backend, first, 4, core.PoolUsage{pgUsagePool: 4}))
	pgApplyBatch(t, s, pgUsageBatch(backend, first, 4, core.PoolUsage{pgUsagePool: 4}))
	if u := pgReadUsage(t, s, backend, pgUsagePeriod); u.APIRequests != 4 || u.Pools[pgUsagePool] != 4 {
		t.Fatalf("after a repeated ID usage = %+v, want it counted once", u)
	}

	pgApplyBatch(t, s, pgUsageBatch(backend, second, 1, core.PoolUsage{pgUsagePool: 1}))
	if u := pgReadUsage(t, s, backend, pgUsagePeriod); u.APIRequests != 5 || u.Pools[pgUsagePool] != 5 {
		t.Errorf("after a new ID usage = %+v, want 5 requests and class_a=5", u)
	}
	if !pgFlushIDRecorded(t, s, first) || !pgFlushIDRecorded(t, s, second) {
		t.Error("both applied flush IDs should be recorded")
	}
}

// TestStoreInt_ApplyUsageBatch_PrunesOldFlushIDs verifies applying a fresh ID
// drops IDs recorded more than 40 days ago and keeps younger ones.
func TestStoreInt_ApplyUsageBatch_PrunesOldFlushIDs(t *testing.T) {
	s := adapterPgStore(t)
	old, recent, fresh := uniqueKey(t, "old"), uniqueKey(t, "recent"), uniqueKey(t, "fresh")
	pgSeedFlushID(t, s, old, 41)
	pgSeedFlushID(t, s, recent, 39)

	pgApplyBatch(t, s, pgUsageBatch(pgUsageBackend(t, s), fresh, 1, nil))

	if pgFlushIDRecorded(t, s, old) {
		t.Error("an ID older than 40 days should be pruned")
	}
	if !pgFlushIDRecorded(t, s, recent) || !pgFlushIDRecorded(t, s, fresh) {
		t.Error("IDs younger than 40 days should be kept")
	}
}

// TestStoreInt_ApplyUsageBatch_FailureWritesNothing verifies a failure at any
// step rolls back the whole batch, flush ID included, so it applies on a retry.
func TestStoreInt_ApplyUsageBatch_FailureWritesNothing(t *testing.T) {
	for name, table := range map[string]string{
		"totals": "backend_usage",
		"pool":   "backend_request_usage",
		"prune":  "usage_flushes",
	} {
		t.Run(name, func(t *testing.T) {
			s := adapterPgStore(t)
			backend := pgUsageBackend(t, s)
			flushID := uniqueKey(t, "flush")
			pgSeedFlushID(t, s, uniqueKey(t, "stale"), 50)
			pgRejectWrites(t, s, table)

			if err := s.ApplyUsageBatch(context.Background(),
				pgUsageBatch(backend, flushID, 3, core.PoolUsage{pgUsagePool: 3})); err == nil {
				t.Fatal("ApplyUsageBatch should fail when a write is rejected")
			}
			if u := pgReadUsage(t, s, backend, pgUsagePeriod); !u.IsZero() {
				t.Errorf("usage = %+v, want nothing written by a failed batch", u)
			}
			if pgFlushIDRecorded(t, s, flushID) {
				t.Error("a failed batch must not record its flush ID")
			}
		})
	}
}

// pgRejectWrites installs a trigger that rejects every insert into table, or
// every delete from usage_flushes, until the test ends.
func pgRejectWrites(t *testing.T, s *Store, table string) {
	t.Helper()
	event := "INSERT"
	if table == "usage_flushes" {
		event = "DELETE"
	}
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION reject_usage_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'rejected'; END $$;
		CREATE TRIGGER reject_usage_write BEFORE `+event+` ON `+table+`
			FOR EACH ROW EXECUTE FUNCTION reject_usage_write();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS reject_usage_write ON `+table+`;
			DROP FUNCTION IF EXISTS reject_usage_write();`)
	})
}

// TestStoreInt_GetUsageForPeriod_ScopedToPeriod pins the monthly rollover:
// budgets reset because the read is keyed by period, with no reset job to run.
func TestStoreInt_GetUsageForPeriod_ScopedToPeriod(t *testing.T) {
	s := adapterPgStore(t)
	backend := pgUsageBackend(t, s)

	pgApplyBatch(t, s, pgUsageBatch(backend, "", 9, core.PoolUsage{pgUsagePool: 9}))

	if u, ok := pgUsageFor(t, s, backend, "2026-06"); ok {
		t.Errorf("backend carried %+v into the next period, want none", u)
	}
}

// pgUsageFor returns backend's usage for period and whether the read held a
// row for it at all.
func pgUsageFor(t *testing.T, s *Store, backend, period string) (core.Usage, bool) {
	t.Helper()
	usage, err := s.GetUsageForPeriod(context.Background(), period)
	if err != nil {
		t.Fatalf("GetUsageForPeriod: %v", err)
	}
	w, ok := usage[backend]
	return w.Total(), ok
}

// TestStoreInt_UsageQueries_SurfaceDatabaseErrors keeps a failing database
// from reading as an unused budget or a flush that succeeded.
func TestStoreInt_UsageQueries_SurfaceDatabaseErrors(t *testing.T) {
	s := adapterPgStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.ApplyUsageBatch(ctx, pgUsageBatch(uniqueKey(t, "backend"), "", 1, nil)); err == nil {
		t.Error("ApplyUsageBatch should surface a cancelled context")
	}
	if _, err := s.GetUsageForPeriod(ctx, pgUsagePeriod); err == nil {
		t.Error("GetUsageForPeriod should surface a cancelled context")
	}
}
