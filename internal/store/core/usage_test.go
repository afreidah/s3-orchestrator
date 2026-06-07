// -------------------------------------------------------------------------------
// Core Usage Reconciliation Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for ReconcileUsage: it must set every backend's
// bytes_used to the ledger truth, report the applied delta, leave agreeing
// backends untouched, drive an emptied backend back to zero, and surface
// read/write errors verbatim. usageTxStub embeds quotaTxStub so only the three
// reconcile primitives carry fixtures.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// usageTxStub drives ReconcileUsage with seeded current/truth maps and records
// the SetBackendBytesUsed writes it receives.
type usageTxStub struct {
	*quotaTxStub
	current    map[string]int64
	truth      map[string]int64
	currentErr error
	truthErr   error
	setErr     error
	sets       map[string]int64
}

func (s *usageTxStub) AllBackendBytesUsed(context.Context) (map[string]int64, error) {
	return s.current, s.currentErr
}

func (s *usageTxStub) SumObjectSizesByBackend(context.Context) (map[string]int64, error) {
	return s.truth, s.truthErr
}

func (s *usageTxStub) SetBackendBytesUsed(_ context.Context, backend string, value int64) error {
	if s.setErr != nil {
		return s.setErr
	}
	if s.sets == nil {
		s.sets = make(map[string]int64)
	}
	s.sets[backend] = value
	return nil
}

func runReconcileUsage(stub *usageTxStub) (map[string]int64, error) {
	if stub.quotaTxStub == nil {
		stub.quotaTxStub = &quotaTxStub{}
	}
	return ReconcileUsage(context.Background(), &stubRunner{tx: stub})
}

// TestReconcileUsage_CorrectsOverAndUnderCount verifies a counter higher than
// the ledger is reduced and one lower is raised, each reported as a signed
// delta, with the authoritative value written.
func TestReconcileUsage_CorrectsOverAndUnderCount(t *testing.T) {
	t.Parallel()
	stub := &usageTxStub{
		current: map[string]int64{"over": 1000, "under": 500},
		truth:   map[string]int64{"over": 600, "under": 800},
	}
	adj, err := runReconcileUsage(stub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adj["over"] != -400 {
		t.Errorf("over delta = %d, want -400", adj["over"])
	}
	if adj["under"] != 300 {
		t.Errorf("under delta = %d, want 300", adj["under"])
	}
	if stub.sets["over"] != 600 || stub.sets["under"] != 800 {
		t.Errorf("sets = %v, want over=600 under=800", stub.sets)
	}
}

// TestReconcileUsage_EmptiedBackendGoesToZero pins the e2 case: a backend with
// no ledger rows is absent from the truth map and must be reset to zero.
func TestReconcileUsage_EmptiedBackendGoesToZero(t *testing.T) {
	t.Parallel()
	stub := &usageTxStub{
		current: map[string]int64{"e2": 162801340},
		truth:   map[string]int64{},
	}
	adj, err := runReconcileUsage(stub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adj["e2"] != -162801340 {
		t.Errorf("e2 delta = %d, want -162801340", adj["e2"])
	}
	if got, ok := stub.sets["e2"]; !ok || got != 0 {
		t.Errorf("e2 set to %d (present=%v), want 0", got, ok)
	}
}

// TestReconcileUsage_NoDriftNoWrites verifies an agreeing backend is neither
// written nor reported.
func TestReconcileUsage_NoDriftNoWrites(t *testing.T) {
	t.Parallel()
	stub := &usageTxStub{
		current: map[string]int64{"c2": 500},
		truth:   map[string]int64{"c2": 500},
	}
	adj, err := runReconcileUsage(stub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(adj) != 0 {
		t.Errorf("expected no adjustments, got %v", adj)
	}
	if len(stub.sets) != 0 {
		t.Errorf("expected no writes, got %v", stub.sets)
	}
}

// TestReconcileUsage_ReadErrorPropagates verifies a failure reading the
// current counters surfaces verbatim and writes nothing.
func TestReconcileUsage_ReadErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("read failed")
	stub := &usageTxStub{currentErr: sentinel}
	_, err := runReconcileUsage(stub)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected read error, got %v", err)
	}
}

// TestReconcileUsage_WriteErrorPropagates verifies a failure applying the
// correction surfaces verbatim.
func TestReconcileUsage_WriteErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("write failed")
	stub := &usageTxStub{
		current: map[string]int64{"b": 100},
		truth:   map[string]int64{"b": 0},
		setErr:  sentinel,
	}
	_, err := runReconcileUsage(stub)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected write error, got %v", err)
	}
}
