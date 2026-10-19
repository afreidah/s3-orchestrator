// -------------------------------------------------------------------------------
// Cleanup Queue Tests
//
// Author: Alex Freidah
//
// Tests for the pure cleanup queue helpers: durationToInterval, the row
// converter, and the claim a settle write matches. The store methods
// themselves are thin DB wrappers tested via integration tests.
// -------------------------------------------------------------------------------

package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// TestDurationToInterval verifies the duration to interval contract.
// Asserts that Microseconds = , want.
func TestDurationToInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		d      time.Duration
		wantUS int64
	}{
		{"one minute", time.Minute, 60_000_000},
		{"one hour", time.Hour, 3_600_000_000},
		{"24 hours", 24 * time.Hour, 86_400_000_000},
		{"zero", 0, 0},
		{"sub-millisecond", 500 * time.Microsecond, 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iv := durationToInterval(tt.d)
			if !iv.Valid {
				t.Error("interval should be valid")
			}
			if iv.Microseconds != tt.wantUS {
				t.Errorf("Microseconds = %d, want %d", iv.Microseconds, tt.wantUS)
			}
		})
	}
}

// queueRow returns a cleanup queue row with every non-claim column set.
func queueRow() db.GetPendingCleanupsRow {
	return db.GetPendingCleanupsRow{
		ID: 7, BackendName: "b1", ObjectKey: "obj", StorageKey: "obj!w1",
		Reason: "delete_failed", Attempts: 3, SizeBytes: 42, Reclaimed: true,
	}
}

// assertQueueFields checks item carries the non-claim columns queueRow sets.
func assertQueueFields(t *testing.T, item *core.CleanupItem) {
	t.Helper()
	want := core.CleanupItem{
		ID: 7, BackendName: "b1", ObjectKey: "obj", StorageKey: "obj!w1",
		Reason: "delete_failed", Attempts: 3, SizeBytes: 42, Reclaimed: true,
	}
	got := *item
	got.Claim = nil
	if got != want {
		t.Errorf("item = %+v, want %+v", got, want)
	}
}

// TestCleanupItemFromRow_Claimed verifies a claimed row converts with its
// stamp and instance as the claim.
func TestCleanupItemFromRow_Claimed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	by := "instance-1"
	row := queueRow()
	row.ClaimedAt = pgtype.Timestamptz{Time: at, Valid: true}
	row.ClaimedBy = &by

	item := cleanupItemFromRow(&row)
	assertQueueFields(t, &item)
	if item.Claim == nil || !item.Claim.At.Equal(at) || item.Claim.By != by {
		t.Errorf("claim = %+v, want {%v %s}", item.Claim, at, by)
	}
}

// TestCleanupItemFromRow_ClaimedWithoutInstance verifies a row claimed by a
// process with no instance id converts with an empty By.
func TestCleanupItemFromRow_ClaimedWithoutInstance(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	row := queueRow()
	row.ClaimedAt = pgtype.Timestamptz{Time: at, Valid: true}

	item := cleanupItemFromRow(&row)
	if item.Claim == nil || !item.Claim.At.Equal(at) || item.Claim.By != "" {
		t.Errorf("claim = %+v, want {%v \"\"}", item.Claim, at)
	}
}

// TestCleanupItemFromRow_Unclaimed verifies a row with no claim time
// converts with a nil claim.
func TestCleanupItemFromRow_Unclaimed(t *testing.T) {
	t.Parallel()
	row := queueRow()
	item := cleanupItemFromRow(&row)
	assertQueueFields(t, &item)
	if item.Claim != nil {
		t.Errorf("claim = %+v, want nil", *item.Claim)
	}
}

// TestClaimOf verifies an item with no claim yields the zero claim and a
// claimed item yields its own claim.
func TestClaimOf(t *testing.T) {
	t.Parallel()
	if got := claimOf(&core.CleanupItem{ID: 1}); got != (core.CleanupClaim{}) {
		t.Errorf("claimOf(nil claim) = %+v, want the zero claim", got)
	}
	want := core.CleanupClaim{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), By: "instance-1"}
	if got := claimOf(&core.CleanupItem{ID: 1, Claim: &want}); got != want {
		t.Errorf("claimOf = %+v, want %+v", got, want)
	}
}
