// -------------------------------------------------------------------------------
// Usage Limits Compilation Tests
//
// Author: Alex Freidah
//
// The compiler turns the pools an operator writes into the lookup admission
// reads on every backend call. Its rules decide what a backend is charged for,
// so each is pinned here: wildcard expansion, unmetered removal, additive
// membership across pools, and the config states it refuses rather than
// resolving in one direction.
// -------------------------------------------------------------------------------

package core

import (
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// poolNames lists the pools an operation charges, for comparison in the
// membership assertions below.
func poolNames(lim UsageLimits, op s3op.Operation) []string {
	pools := lim.PoolsFor(op)
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		names = append(names, p.Name)
	}
	return names
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestNewUsageLimits_WildcardCoversEveryMeteredOperation pins the desugaring a
// bare api_request_limit relies on: one pool that charges everything.
func TestNewUsageLimits_WildcardCoversEveryMeteredOperation(t *testing.T) {
	t.Parallel()
	lim, err := NewUsageLimits(0, 0, SingleRequestPool(5000), nil)
	if err != nil {
		t.Fatalf("NewUsageLimits: %v", err)
	}
	for _, op := range s3op.All() {
		if got := poolNames(lim, op); !slices.Equal(got, []string{PoolAll}) {
			t.Errorf("%s charges %v, want [%s]", op, got, PoolAll)
		}
	}
}

// TestNewUsageLimits_UnmeteredIsChargedToNothing is the free-operation case:
// GCS does not bill deletes, so a delete must charge no budget - including the
// wildcard pool, which would otherwise swallow it.
func TestNewUsageLimits_UnmeteredIsChargedToNothing(t *testing.T) {
	t.Parallel()
	lim, err := NewUsageLimits(0, 0, SingleRequestPool(5000), []s3op.Operation{s3op.DeleteObject})
	if err != nil {
		t.Fatalf("NewUsageLimits: %v", err)
	}
	if got := poolNames(lim, s3op.DeleteObject); len(got) != 0 {
		t.Errorf("DeleteObject charges %v, want no pools", got)
	}
	if got := poolNames(lim, s3op.PutObject); !slices.Equal(got, []string{PoolAll}) {
		t.Errorf("PutObject charges %v, want [%s]", got, PoolAll)
	}
}

// TestNewUsageLimits_OperationChargesEveryPoolContainingIt covers the additive
// rule that makes a sub-cap inside an aggregate cap expressible.
func TestNewUsageLimits_OperationChargesEveryPoolContainingIt(t *testing.T) {
	t.Parallel()
	lim, err := NewUsageLimits(0, 0, []PoolSpec{
		{Name: "everything", Operations: []string{s3op.Wildcard}, Limit: 1000},
		{Name: "lists", Operations: []string{string(s3op.ListObjects)}, Limit: 10},
	}, nil)
	if err != nil {
		t.Fatalf("NewUsageLimits: %v", err)
	}
	if got := poolNames(lim, s3op.ListObjects); !slices.Equal(got, []string{"everything", "lists"}) {
		t.Errorf("ListObjects charges %v, want both pools in configured order", got)
	}
	if got := poolNames(lim, s3op.GetObject); !slices.Equal(got, []string{"everything"}) {
		t.Errorf("GetObject charges %v, want [everything]", got)
	}
}

// TestNewUsageLimits_RefusesPoolChargingAnUnmeteredOperation covers the one
// config state with no defensible reading: an operation cannot be both free
// and budgeted, and picking a winner silently would hide the mistake.
func TestNewUsageLimits_RefusesPoolChargingAnUnmeteredOperation(t *testing.T) {
	t.Parallel()
	_, err := NewUsageLimits(0, 0, []PoolSpec{
		{Name: "writes", Operations: []string{string(s3op.DeleteObject)}, Limit: 10},
	}, []s3op.Operation{s3op.DeleteObject})
	if err == nil {
		t.Fatal("a pool charging an unmetered operation must be refused")
	}
}

// TestNewUsageLimits_DuplicateOperationChargesOnce pins that listing an
// operation twice in one pool does not double its cost.
func TestNewUsageLimits_DuplicateOperationChargesOnce(t *testing.T) {
	t.Parallel()
	lim, err := NewUsageLimits(0, 0, []PoolSpec{
		{Name: "reads", Operations: []string{
			string(s3op.GetObject), string(s3op.GetObject), s3op.Wildcard,
		}, Limit: 10},
	}, nil)
	if err != nil {
		t.Fatalf("NewUsageLimits: %v", err)
	}
	if got := poolNames(lim, s3op.GetObject); !slices.Equal(got, []string{"reads"}) {
		t.Errorf("GetObject charges %v, want [reads] once", got)
	}
}

// TestUsageLimits_Unlimited covers the fast path admission short-circuits on,
// including the pool that is counted but never refuses.
func TestUsageLimits_Unlimited(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		egress  int64
		ingress int64
		specs   []PoolSpec
		want    bool
	}{
		{"nothing configured", 0, 0, nil, true},
		{"egress bounded", 100, 0, nil, false},
		{"ingress bounded", 0, 100, nil, false},
		{"pool bounded", 0, 0, SingleRequestPool(10), false},
		{"pool present but unlimited", 0, 0, []PoolSpec{
			{Name: PoolAll, Operations: []string{s3op.Wildcard}, Limit: 0},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lim, err := NewUsageLimits(tc.egress, tc.ingress, tc.specs, nil)
			if err != nil {
				t.Fatalf("NewUsageLimits: %v", err)
			}
			if got := lim.Unlimited(); got != tc.want {
				t.Errorf("Unlimited() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSingleRequestPool_UnsetCapCompilesToNoPools keeps an absent
// api_request_limit from becoming a pool that reports as configured.
func TestSingleRequestPool_UnsetCapCompilesToNoPools(t *testing.T) {
	t.Parallel()
	for _, limit := range []int64{0, -1} {
		if got := SingleRequestPool(limit); got != nil {
			t.Errorf("SingleRequestPool(%d) = %+v, want nil", limit, got)
		}
	}
}
