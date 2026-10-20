// -------------------------------------------------------------------------------
// Postgres Notification Outbox Index Migration Integration Test
//
// Author: Alex Freidah
//
// The pending outbox index covers every row, since the notifier itself drops
// a row once its endpoint's retries are spent. Rolling back restores the
// partial index, and upgrading again replaces it.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

// partialOutboxIndexVersion is the last schema whose pending outbox index
// skipped rows by attempt count.
const partialOutboxIndexVersion = 44

// TestMigrations_PendingOutboxIndexCoversEveryRow verifies the upgrade leaves
// an unfiltered pending index, the rollback restores the partial one, and a
// second upgrade replaces it again.
func TestMigrations_PendingOutboxIndexCoversEveryRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := freshDatabase(ctx, t)

	steps := []struct {
		version int64
		partial bool
	}{
		{ExpectedSchemaVersion, false},
		{partialOutboxIndexVersion, true},
		{ExpectedSchemaVersion, false},
	}
	for _, step := range steps {
		if step.partial {
			migrateDownTo(ctx, t, s.connStr, step.version)
		} else {
			migrateTo(ctx, t, s.connStr, step.version)
		}
		def := pendingOutboxIndexDef(ctx, t, s)
		if strings.Contains(def, "WHERE") != step.partial {
			t.Errorf("at version %d index = %q, want partial=%v", step.version, def, step.partial)
		}
	}
}

// pendingOutboxIndexDef returns the definition Postgres reports for the
// pending outbox index.
func pendingOutboxIndexDef(ctx context.Context, t *testing.T, s *Store) string {
	t.Helper()
	var def string
	if err := s.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_notification_outbox_pending'`).Scan(&def); err != nil {
		t.Fatalf("read pending outbox index: %v", err)
	}
	return def
}
