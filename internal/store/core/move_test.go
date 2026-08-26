// -------------------------------------------------------------------------------
// Core MoveObjectLocation Tests
//
// Author: Alex Freidah
//
// A move relocates stored bytes without re-encoding them, so everything the
// source row said about those bytes has to land on the destination row. The
// compression measurement is the piece that does not ride through StoredForm:
// it is a fact about what the encoder produced rather than a description of the
// bytes, so it is carried separately and can be dropped separately. Losing it
// costs the next compression pass a download and an encode to learn it again.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// moveTxStub serves one locked source row and records what the move writes.
type moveTxStub struct {
	*quotaTxStub
	src       *ObjectLocation
	targetHas bool
	probeErr  error
	probes    []*CompressionProbe
	inserted  []*ObjectLocation
	deleted   []string
}

// newMoveStub builds a stub holding src as the only copy, with the embedded
// quota stub initialized.
func newMoveStub(src *ObjectLocation) *moveTxStub {
	return &moveTxStub{quotaTxStub: &quotaTxStub{}, src: src}
}

// CheckObjectExistsOnBackend reports whether the destination already holds a copy.
func (s *moveTxStub) CheckObjectExistsOnBackend(context.Context, string, string) (bool, error) {
	return s.targetHas, nil
}

// LockObjectOnBackend serves the seeded source row.
func (s *moveTxStub) LockObjectOnBackend(context.Context, string, string) (*ObjectLocation, bool, error) {
	return s.src, s.src != nil, nil
}

// DeleteObjectFromBackend records the backend the copy left.
func (s *moveTxStub) DeleteObjectFromBackend(_ context.Context, _, backend string) error {
	s.deleted = append(s.deleted, backend)
	return nil
}

// InsertObjectLocation records the destination row the move wrote.
func (s *moveTxStub) InsertObjectLocation(_ context.Context, loc *ObjectLocation) error {
	s.inserted = append(s.inserted, loc)
	return nil
}

// RecordCompressionProbe records the carried measurement or fails the move.
func (s *moveTxStub) RecordCompressionProbe(_ context.Context, probe *CompressionProbe) error {
	if s.probeErr != nil {
		return s.probeErr
	}
	s.probes = append(s.probes, probe)
	return nil
}

// probedSource is a copy carrying a recorded measurement, which is what every
// object a compression pass declined on ratio looks like.
func probedSource() *ObjectLocation {
	return &ObjectLocation{
		ObjectKey:             "k",
		BackendName:           "b1",
		SizeBytes:             4096,
		CompressionProbeSize:  4000,
		CompressionProbeLevel: "default",
	}
}

// runMove invokes MoveObjectLocation against the stub.
func runMove(stub *moveTxStub) (int64, error) {
	return MoveObjectLocation(context.Background(), &stubRunner{tx: stub}, "k", "b1", "b2")
}

// TestMoveObjectLocation_CarriesTheProbe verifies the source's measurement is
// written against the destination row. The move is verbatim, so what the
// encoder produced for these bytes on b1 still holds on b2; dropping it puts
// the copy back in the compression listing to be downloaded and encoded again.
func TestMoveObjectLocation_CarriesTheProbe(t *testing.T) {
	t.Parallel()
	stub := newMoveStub(probedSource())

	moved, err := runMove(stub)
	if err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if moved != 4096 {
		t.Errorf("moved %d bytes, want 4096", moved)
	}
	if len(stub.probes) != 1 {
		t.Fatalf("recorded %d probes, want 1", len(stub.probes))
	}
	got := stub.probes[0]
	if got.ObjectKey != "k" || got.BackendName != "b2" {
		t.Errorf("probe written for %s/%s, want k/b2", got.ObjectKey, got.BackendName)
	}
	if got.Size != 4000 || got.Level != "default" {
		t.Errorf("probe = {%d, %q}, want {4000, default}", got.Size, got.Level)
	}
}

// TestMoveObjectLocation_NoProbeWritesNothing verifies a copy that was never
// measured does not have an empty measurement written for it. A recorded size
// of zero would read as an encoder result of nothing and exclude the copy from
// every future compression pass.
func TestMoveObjectLocation_NoProbeWritesNothing(t *testing.T) {
	t.Parallel()
	src := probedSource()
	src.CompressionProbeSize = 0
	src.CompressionProbeLevel = ""
	stub := newMoveStub(src)

	if _, err := runMove(stub); err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if len(stub.probes) != 0 {
		t.Errorf("recorded %d probes, want none for an unmeasured copy", len(stub.probes))
	}
	if len(stub.inserted) != 1 {
		t.Errorf("inserted %d rows, want 1: the move itself still happens", len(stub.inserted))
	}
}

// TestMoveObjectLocation_ProbeFailureAbortsTheMove verifies a failure to carry
// the measurement surfaces rather than being swallowed, so the transaction
// rolls back. Committing without it would leave a destination row that the next
// compression pass pays to re-measure, silently and fleet-wide.
func TestMoveObjectLocation_ProbeFailureAbortsTheMove(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("record probe failed")
	stub := newMoveStub(probedSource())
	stub.probeErr = sentinel

	moved, err := runMove(stub)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the probe failure", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 on a failed move", moved)
	}
	// The quota deltas are applied after the probe, so a move that aborts here
	// must not have moved any capacity between the two backends.
	if len(stub.ops) != 0 {
		t.Errorf("quota ops = %v, want none: the move aborted before the deltas", stub.ops)
	}
}

// TestMoveObjectLocation_NoOpWhenTargetHasCopy pins the benign no-op: nothing is
// deleted, inserted, or measured when the destination already holds the object.
func TestMoveObjectLocation_NoOpWhenTargetHasCopy(t *testing.T) {
	t.Parallel()
	stub := newMoveStub(probedSource())
	stub.targetHas = true

	moved, err := runMove(stub)
	if err != nil || moved != 0 {
		t.Fatalf("moved = %d, err = %v; want 0, nil", moved, err)
	}
	if len(stub.deleted) != 0 || len(stub.inserted) != 0 || len(stub.probes) != 0 {
		t.Errorf("stub mutated: deleted=%v inserted=%d probes=%d",
			stub.deleted, len(stub.inserted), len(stub.probes))
	}
}
