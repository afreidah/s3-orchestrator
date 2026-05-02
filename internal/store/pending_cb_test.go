// -------------------------------------------------------------------------------
// Pending Store - Circuit Breaker Forwarder Tests
//
// Author: Alex Freidah
//
// Verifies every PendingStore method on the circuit-breaker decorator
// forwards arguments to the inner store and returns ErrDBUnavailable
// while the shared breaker is open.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCBForwarders_PendingStore verifies every PendingStore method on the
// circuit-breaker decorator forwards arguments to the inner store and
// returns the inner store's result unchanged when the breaker is closed.
func TestCBForwarders_PendingStore(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		pendingDepthResp:        7,
		getStalePendingResp:     []PendingObject{{IntentID: "x"}},
		promotePendingResult:    PendingPromoteCommitted,
		promotePendingDisplaced: []DeletedCopy{{BackendName: "old", SizeBytes: 5}},
	}
	cb := newTestCB(mock, 3, time.Minute)
	ctx := context.Background()

	if err := cb.InsertPending(ctx, &PendingObject{IntentID: "x"}); err != nil {
		t.Errorf("InsertPending: %v", err)
	}
	if err := cb.DeletePending(ctx, "x"); err != nil {
		t.Errorf("DeletePending: %v", err)
	}
	rows, err := cb.GetStalePending(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	if len(rows) != 1 || rows[0].IntentID != "x" {
		t.Errorf("GetStalePending = %+v, want one row with IntentID x", rows)
	}
	depth, err := cb.PendingDepth(ctx)
	if err != nil {
		t.Fatalf("PendingDepth: %v", err)
	}
	if depth != 7 {
		t.Errorf("PendingDepth = %d, want 7", depth)
	}
	result, displaced, err := cb.PromotePending(ctx, &PendingObject{IntentID: "x"})
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != PendingPromoteCommitted {
		t.Errorf("PromotePending result = %v, want Committed", result)
	}
	if len(displaced) != 1 || displaced[0].BackendName != "old" {
		t.Errorf("PromotePending displaced = %+v", displaced)
	}
	if err := cb.DeletePendingByBackend(ctx, "b1"); err != nil {
		t.Errorf("DeletePendingByBackend: %v", err)
	}
}

// TestCBForwarders_PendingStore_OpenCircuitReturnsSentinel verifies that
// once the shared breaker is open, every PendingStore method returns
// ErrDBUnavailable without touching the inner store.
func TestCBForwarders_PendingStore_OpenCircuitReturnsSentinel(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	// Trip the breaker via getAllLocations, then verify pending methods
	// short-circuit with ErrDBUnavailable instead of hitting the inner mock.
	mock := &mockStore{getAllLocationsErr: dbErr}
	cb := newTestCB(mock, 1, time.Minute)
	ctx := context.Background()

	_, _ = cb.GetAllObjectLocations(ctx, "k")

	if err := cb.InsertPending(ctx, &PendingObject{IntentID: "x"}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("InsertPending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePending(ctx, "x"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("DeletePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.GetStalePending(ctx, time.Now(), 10); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("GetStalePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.PendingDepth(ctx); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("PendingDepth: got %v, want ErrDBUnavailable", err)
	}
	if _, _, err := cb.PromotePending(ctx, &PendingObject{IntentID: "x"}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("PromotePending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePendingByBackend(ctx, "b1"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("DeletePendingByBackend: got %v, want ErrDBUnavailable", err)
	}
}
