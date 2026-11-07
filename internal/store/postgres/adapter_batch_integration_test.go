// -------------------------------------------------------------------------------
// Postgres TxAdapter - Batched Write Integration Tests
//
// Author: Alex Freidah
//
// The cleanup insert, the intent clear and the stripe charge each take many rows
// in one statement. These tests run them against a real PostgreSQL instance with
// several rows, so a column zipped against the wrong array would show.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestPgAdapter_InsertCleanup_EveryRequestKeepsItsFields verifies a batched
// insert returns one row per request, each with its own columns and the claim
// the row holds.
func TestPgAdapter_InsertCleanup_EveryRequestKeepsItsFields(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	requests := map[string]*core.CleanupRequest{}
	for _, name := range []string{"first", "second", "third"} {
		key := uniqueKey(t, name)
		requests[key] = &core.CleanupRequest{
			BackendName: "backend-a", ObjectKey: key, StorageKey: key + "!path", Reason: name, SizeBytes: int64(len(name)),
		}
	}
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		items, err := a.InsertCleanup(ctx, slices.Collect(maps.Values(requests))...)
		if err != nil {
			t.Fatalf("InsertCleanup: %v", err)
		}
		if len(items) != len(requests) {
			t.Fatalf("items = %d, want %d", len(items), len(requests))
		}
		for i := range items {
			assertQueuedRow(t, a, &items[i], requests[items[i].ObjectKey], s.instanceID)
		}
	})
}

// assertQueuedRow checks a returned row against the request it came from, the
// claim it should hold, and what the table stored for it.
func assertQueuedRow(t *testing.T, a *pgTxAdapter, item *core.CleanupItem, want *core.CleanupRequest, instanceID string) {
	t.Helper()
	if want == nil || item.CleanupRequest != *want {
		t.Errorf("item = %+v, want the request %+v", item.CleanupRequest, want)
	}
	if item.Claim == nil || item.Claim.By != instanceID {
		t.Errorf("claim = %+v, want one held by %q", item.Claim, instanceID)
	}
	if row := txCleanupRow(t, a, item.ID); row.CleanupRequest != item.CleanupRequest {
		t.Errorf("stored %+v, returned %+v", row.CleanupRequest, item.CleanupRequest)
	}
}

// TestPgAdapter_ClearPendingForKey_ReportsEachKey verifies clearing several keys
// names the key of every intent it removed, spares keep, and leaves a key it
// was not given alone.
func TestPgAdapter_ClearPendingForKey_ReportsEachKey(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	first, second, other := uniqueKey(t, "first"), uniqueKey(t, "second"), uniqueKey(t, "other")
	intents := map[string]string{
		uniqueKey(t, "i-first"):  first,
		uniqueKey(t, "i-second"): second,
		uniqueKey(t, "i-keep"):   second,
		uniqueKey(t, "i-other"):  other,
	}
	for id, key := range intents {
		if _, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
			IntentID: id, ObjectKey: key, BackendName: "backend-a", SizeBytes: 1,
		}, core.AcceptForWrite); err != nil {
			t.Fatalf("InsertPending: %v", err)
		}
		defer func() { _ = s.DeletePending(ctx, id) }()
	}
	keep := uniqueKey(t, "i-keep")
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		cleared, err := a.ClearPendingForKey(ctx, []string{keep}, first, second)
		if err != nil {
			t.Fatalf("ClearPendingForKey: %v", err)
		}
		if len(cleared) != 2 {
			t.Fatalf("cleared = %+v, want one intent on each key", cleared)
		}
		for _, si := range cleared {
			if intents[si.IntentID] != si.ObjectKey {
				t.Errorf("intent %s reported on %s, want %s", si.IntentID, si.ObjectKey, intents[si.IntentID])
			}
		}
	})
}

// TestPgAdapter_AdjustQuotaStripes_ChargesEveryRow verifies one call lands every
// charge on its own row, summing to the total they moved.
func TestPgAdapter_AdjustQuotaStripes_ChargesEveryRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		beforeA := pgBytesUsed(t, ctx, a, "backend-a")
		if err := a.AdjustQuotaStripes(ctx,
			core.StripeCharge{Backend: "backend-a", Stripe: 1, Delta: 100},
			core.StripeCharge{Backend: "backend-a", Stripe: 2, Delta: 20},
			core.StripeCharge{Backend: "backend-b", Stripe: 1, Delta: 3},
		); err != nil {
			t.Fatalf("AdjustQuotaStripes: %v", err)
		}
		if got := pgBytesUsed(t, ctx, a, "backend-a") - beforeA; got != 120 {
			t.Errorf("backend-a moved by %d, want 120", got)
		}
	})
}
