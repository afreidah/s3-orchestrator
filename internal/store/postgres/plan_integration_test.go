//go:build integration

// -------------------------------------------------------------------------------
// Query Plan Checks
//
// Author: Alex Freidah
//
// The helper the plan-shape tests share, and the check that backend_capacity's
// per-backend sums run from the covering indexes. A sum that drops back to
// reading the table shows up nowhere else as a failure, only as a slower
// admission check on every write.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// planQuerier is what explainPlan runs EXPLAIN on: the pool, or one connection
// whose planner settings a test changed.
type planQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// explainPlan runs EXPLAIN with options on query and returns the plan text,
// one line per plan node.
func explainPlan(ctx context.Context, t *testing.T, q planQuerier, options, query string, args ...any) string {
	t.Helper()
	rows, err := q.Query(ctx, "EXPLAIN ("+options+") "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan.String()
}

// capacityPlanBackend owns the rows the capacity plan test seeds, so they can
// be removed without touching what other tests wrote.
const capacityPlanBackend = "capacity-plan"

// TestBackendCapacity_SumsFromCoveringIndexes seeds queued, dead-lettered and
// in-flight rows for one backend and asserts the view's three sums read only
// their covering indexes. Sequential scans are disabled because on a test-sized
// table they are genuinely cheaper; what matters is that the indexes can serve
// the sums without the table, which a size_bytes missing from them would undo.
func TestBackendCapacity_SumsFromCoveringIndexes(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	seedCapacityRows(ctx, t, s, 2000)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_bitmapscan = off`); err != nil {
		t.Fatalf("disable bitmap scan: %v", err)
	}

	plan := explainPlan(ctx, t, conn.Conn(), "COSTS OFF",
		`SELECT orphan_bytes, inflight_bytes FROM backend_capacity WHERE backend_name = $1`, capacityPlanBackend)
	for _, index := range []string{
		"idx_cleanup_queue_backend_storage_key",
		"idx_cleanup_dlq_backend_storage_key",
		"idx_pending_objects_backend_storage_key",
	} {
		if !strings.Contains(plan, "Index Only Scan using "+index) {
			t.Errorf("the capacity sum does not run from %s alone; size_bytes is missing from it.\n%s", index, plan)
		}
	}
}

// seedCapacityRows gives capacityPlanBackend n queued, n dead-lettered and n
// in-flight rows and removes them when the test ends. The VACUUM sets the
// visibility map an index-only scan needs.
func seedCapacityRows(ctx context.Context, t *testing.T, s *Store, n int) {
	t.Helper()
	t.Cleanup(func() { dropCapacityRows(t, s) })
	for _, stmt := range []string{
		`INSERT INTO backend_quotas (backend_name, bytes_limit)
		 SELECT $2::text, 0 WHERE $1::int > 0 ON CONFLICT (backend_name) DO NOTHING`,
		`INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes)
		 SELECT $2::text, 'k' || g, 'k' || g, 'plan', g FROM generate_series(1, $1::int) g`,
		`INSERT INTO cleanup_dlq (original_id, backend_name, object_key, storage_key, reason, size_bytes, attempts, first_enqueued_at)
		 SELECT g, $2::text, 'k' || g, 'k' || g, 'plan', g, 10, NOW() FROM generate_series(1, $1::int) g`,
		`INSERT INTO pending_objects (intent_id, object_key, storage_key, backend_name, size_bytes)
		 SELECT 'plan-' || g, 'k' || g, 'k' || g, $2::text, g FROM generate_series(1, $1::int) g`,
	} {
		if _, err := s.pool.Exec(ctx, stmt, n, capacityPlanBackend); err != nil {
			t.Fatalf("seed capacity rows: %v", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE cleanup_queue, cleanup_dlq, pending_objects`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
}

// dropCapacityRows removes the seed and the backend it belongs to.
func dropCapacityRows(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"cleanup_queue", "cleanup_dlq", "pending_objects", "backend_quotas"} {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM `+pgx.Identifier{table}.Sanitize()+` WHERE backend_name = $1`, capacityPlanBackend); err != nil {
			t.Fatalf("drop seeded %s rows: %v", table, err)
		}
	}
}
