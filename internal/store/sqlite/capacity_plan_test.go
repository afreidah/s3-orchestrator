// -------------------------------------------------------------------------------
// Capacity Sum Plan Check
//
// Author: Alex Freidah
//
// backend_capacity sums size_bytes per backend over cleanup_queue, cleanup_dlq
// and pending_objects on every admission check. Each sum is served by a
// covering index; one that reads the table instead shows up nowhere else as a
// failure, only as a slower admission check on every write.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"strings"
	"testing"
)

// TestBackendCapacity_SumsFromCoveringIndexes asserts the view's three
// per-backend sums each read a covering index rather than the table.
func TestBackendCapacity_SumsFromCoveringIndexes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	rows, err := s.db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT orphan_bytes, inflight_bytes FROM backend_capacity WHERE backend_name = ?`, "b1")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}

	for _, index := range []string{
		"idx_cleanup_queue_backend_storage_key",
		"idx_cleanup_dlq_backend_storage_key",
		"idx_pending_objects_backend_storage_key",
	} {
		if !strings.Contains(plan.String(), "USING COVERING INDEX "+index) {
			t.Errorf("the capacity sum does not run from %s alone; size_bytes is missing from it.\n%s", index, plan.String())
		}
	}
}
