// -------------------------------------------------------------------------------
// SQLite TxAdapter - Batched Write Tests
//
// Author: Alex Freidah
//
// The cleanup insert, the intent clear and the stripe charge each take many rows
// in one call, so the engine-agnostic transactions behave the same on both
// stores. These tests run them with several rows.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestAdapter_InsertCleanup_EveryRequestKeepsItsFields verifies a batched insert
// returns one claimed row per request, in the order given.
func TestAdapter_InsertCleanup_EveryRequestKeepsItsFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	requests := []*core.CleanupRequest{
		{BackendName: "backend-a", ObjectKey: "k1", StorageKey: "p1", Reason: "first", SizeBytes: 1},
		{BackendName: "backend-a", ObjectKey: "k2", StorageKey: "p2", Reason: "second", SizeBytes: 2},
	}
	items, err := s.direct().InsertCleanup(context.Background(), requests...)
	if err != nil {
		t.Fatalf("InsertCleanup: %v", err)
	}
	if len(items) != len(requests) {
		t.Fatalf("items = %d, want %d", len(items), len(requests))
	}
	for i := range items {
		if items[i].CleanupRequest != *requests[i] || items[i].ID == 0 || items[i].Claim == nil {
			t.Errorf("items[%d] = %+v, want the request with an id and a claim", i, items[i])
		}
	}
}

// TestSqlite_ClearPendingForKey_ReportsEachKey verifies clearing several keys
// names the key of every intent it removed and leaves a key it was not given.
func TestSqlite_ClearPendingForKey_ReportsEachKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedPendingIntent(t, s, "i-1", "bucket/k1", "backend-a", 1)
	seedPendingIntent(t, s, "i-2", "bucket/k2", "backend-a", 2)
	seedPendingIntent(t, s, "i-3", "bucket/other", "backend-a", 3)

	withAdapter(t, s, func(a *sqliteTxAdapter) {
		cleared, err := a.ClearPendingForKey(context.Background(), nil, "bucket/k1", "bucket/k2")
		if err != nil {
			t.Fatalf("ClearPendingForKey: %v", err)
		}
		want := map[string]string{"i-1": "bucket/k1", "i-2": "bucket/k2"}
		if len(cleared) != len(want) {
			t.Fatalf("cleared = %+v, want one intent on each key", cleared)
		}
		for _, si := range cleared {
			if want[si.IntentID] != si.ObjectKey {
				t.Errorf("intent %s reported on %s", si.IntentID, si.ObjectKey)
			}
		}
		if got := countPendingForKey(t, a, "bucket/other"); got != 1 {
			t.Errorf("intents left on the other key = %d, want 1", got)
		}
	})
}

// TestAdapter_AdjustQuotaStripes_ChargesEveryRow verifies one call lands every
// charge on its own row.
func TestAdapter_AdjustQuotaStripes_ChargesEveryRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	withAdapter(t, s, func(a *sqliteTxAdapter) {
		if err := a.AdjustQuotaStripes(context.Background(),
			core.StripeCharge{Backend: "backend-a", Stripe: 1, Delta: 100},
			core.StripeCharge{Backend: "backend-a", Stripe: 2, Delta: 20},
		); err != nil {
			t.Fatalf("AdjustQuotaStripes: %v", err)
		}
		if got := stripeBytes(t, a, "backend-a", 1); got != 100 {
			t.Errorf("stripe 1 = %d, want 100", got)
		}
		if got := stripeBytes(t, a, "backend-a", 2); got != 20 {
			t.Errorf("stripe 2 = %d, want 20", got)
		}
	})
}
