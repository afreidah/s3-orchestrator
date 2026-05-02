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

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestCBForwarders_GetObjectBackendsForKeys verifies the new ObjectStore
// method routes through the breaker: forwarded under closed breaker,
// short-circuits to ErrDBUnavailable once the breaker has tripped.
func TestCBForwarders_GetObjectBackendsForKeys(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		getBackendsForKeysResp: map[string][]string{"k1": {"b1", "b2"}},
	}
	cb := newTestCB(mock, 3, time.Minute)
	got, err := cb.GetObjectBackendsForKeys(context.Background(), []string{"k1"})
	if err != nil {
		t.Fatalf("GetObjectBackendsForKeys: %v", err)
	}
	if len(got["k1"]) != 2 || got["k1"][0] != "b1" || got["k1"][1] != "b2" {
		t.Errorf("forwarder did not return mock response verbatim: %+v", got)
	}

	// Trip the breaker through a different ObjectStore method, then
	// verify GetObjectBackendsForKeys short-circuits.
	dbErr := errors.New("connection refused")
	tripMock := &mockStore{getAllLocationsErr: dbErr}
	tripCB := newTestCB(tripMock, 1, time.Minute)
	_, _ = tripCB.GetAllObjectLocations(context.Background(), "k")
	if _, err := tripCB.GetObjectBackendsForKeys(context.Background(), []string{"k1"}); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("expected ErrDBUnavailable when breaker open, got %v", err)
	}
}

// TestCBForwarders_PendingStore verifies every PendingStore method on the
// circuit-breaker decorator forwards arguments to the inner store and
// returns the inner store's result unchanged when the breaker is closed.
func TestCBForwarders_PendingStore(t *testing.T) {
	t.Parallel()
	mock := &mockStore{
		pendingDepthResp:        7,
		getStalePendingResp:     []core.PendingObject{{IntentID: "x"}},
		promotePendingResult:    core.PendingPromoteCommitted,
		promotePendingDisplaced: []core.DeletedCopy{{BackendName: "old", SizeBytes: 5}},
	}
	cb := newTestCB(mock, 3, time.Minute)
	ctx := context.Background()

	if err := cb.InsertPending(ctx, &core.PendingObject{IntentID: "x"}); err != nil {
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
	result, displaced, err := cb.PromotePending(ctx, &core.PendingObject{IntentID: "x"})
	if err != nil {
		t.Fatalf("PromotePending: %v", err)
	}
	if result != core.PendingPromoteCommitted {
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

	if err := cb.InsertPending(ctx, &core.PendingObject{IntentID: "x"}); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("InsertPending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePending(ctx, "x"); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("DeletePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.GetStalePending(ctx, time.Now(), 10); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("GetStalePending: got %v, want ErrDBUnavailable", err)
	}
	if _, err := cb.PendingDepth(ctx); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("PendingDepth: got %v, want ErrDBUnavailable", err)
	}
	if _, _, err := cb.PromotePending(ctx, &core.PendingObject{IntentID: "x"}); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("PromotePending: got %v, want ErrDBUnavailable", err)
	}
	if err := cb.DeletePendingByBackend(ctx, "b1"); !errors.Is(err, core.ErrDBUnavailable) {
		t.Errorf("DeletePendingByBackend: got %v, want ErrDBUnavailable", err)
	}
}
