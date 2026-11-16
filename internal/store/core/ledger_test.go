// -------------------------------------------------------------------------------
// Ledger Stats Tests
//
// Author: Alex Freidah
//
// Covers how the ledger's groups fold into figures, the per-backend and
// fleet-wide views of them, and the coverage split that keeps unreachable
// backends out of the verification age.
// -------------------------------------------------------------------------------

package core

import (
	"maps"
	"testing"
	"time"
)

// ledgerFixture is two reachable backends and one the sweep cannot read.
func ledgerFixture() LedgerStats {
	return LedgerStats{
		"a": {Objects: 10, Unhashed: 2, Plaintext: 3, Unreadable: 1, Verifiable: 8, NeverVerified: 2,
			Compressed: CompressionStat{Objects: 4, LogicalBytes: 400, StoredBytes: 100}},
		"b": {Objects: 5, Unhashed: 1, Plaintext: 1, Verifiable: 4, NeverVerified: 1},
		"c": {Objects: 7, Verifiable: 6, NeverVerified: 6},
	}
}

// TestLedgerStats_Views verifies each per-backend map and fleet total reads the
// figure it names.
func TestLedgerStats_Views(t *testing.T) {
	t.Parallel()
	s := ledgerFixture()

	if got := s.ObjectCounts(); !maps.Equal(got, map[string]int64{"a": 10, "b": 5, "c": 7}) {
		t.Errorf("ObjectCounts = %v", got)
	}
	if got := s.UnhashedCounts(); !maps.Equal(got, map[string]int64{"a": 2, "b": 1, "c": 0}) {
		t.Errorf("UnhashedCounts = %v", got)
	}
	if got := s.PlaintextCopies(); got != 4 {
		t.Errorf("PlaintextCopies = %d, want 4", got)
	}
	if got := s.UnreadableCopies(); got != 1 {
		t.Errorf("UnreadableCopies = %d, want 1", got)
	}
	want := map[string]CompressionStat{"a": {Objects: 4, LogicalBytes: 400, StoredBytes: 100}}
	if got := s.Compression(); !maps.Equal(got, want) {
		t.Errorf("Compression = %v, want only backend a", got)
	}
}

// TestLedgerStats_CoverageSplitsByReachability verifies the never-verified
// count comes from reachable backends only, the unreachable backend's
// verifiable copies are deferred instead, and the age is measured from the
// oldest touch the caller read.
func TestLedgerStats_CoverageSplitsByReachability(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cov := ledgerFixture().Coverage([]string{"a", "b"}, now.Add(-2*time.Hour), now)
	if cov.OldestUnverifiedAge != 2*time.Hour {
		t.Errorf("age = %v, want 2h", cov.OldestUnverifiedAge)
	}
	if cov.NeverVerified != 3 || cov.Deferred != 6 {
		t.Errorf("never verified = %d, deferred = %d; want 3 and 6", cov.NeverVerified, cov.Deferred)
	}
}

// TestLedgerStats_CoverageWithNothingVerifiable verifies a fleet with no
// verifiable copy on a reachable backend, so no oldest touch, reports a zero
// age rather than one measured from the zero time.
func TestLedgerStats_CoverageWithNothingVerifiable(t *testing.T) {
	t.Parallel()
	s := LedgerStats{"a": {Objects: 3}}
	if cov := s.Coverage([]string{"a"}, time.Time{}, time.Now()); cov != (CoverageStat{}) {
		t.Errorf("coverage = %+v, want zero", cov)
	}
}

// TestFoldLedger_EveryFigure folds one group per interesting form on backend a
// and checks each figure counts the groups its definition names: unmanaged
// hashed rows are not verifiable, an encrypted row with no key is unreadable
// while a key with no envelope is plaintext, and only compressed groups feed
// the compression figures.
func TestFoldLedger_EveryFigure(t *testing.T) {
	t.Parallel()
	groups := []LedgerGroup{
		{Backend: "a", Form: 0, Managed: true, Copies: 1, StoredBytes: 10, ObjectBytes: 10},
		{Backend: "a", Form: StoredHashed, Managed: true, Copies: 2, NeverScrubbed: 1},
		{Backend: "a", Form: StoredHashed, Managed: false, Copies: 4, NeverScrubbed: 4},
		{Backend: "a", Form: StoredEncrypted | StoredHashed, Managed: true, Copies: 8},
		{Backend: "a", Form: StoredKeyed, Managed: true, Copies: 16},
		{Backend: "a", Form: StoredReadable | StoredCompressed | StoredHashed, Managed: true,
			Copies: 32, StoredBytes: 100, ObjectBytes: 400, NeverScrubbed: 2},
		{Backend: "b", Form: StoredCompressed, Managed: true, Copies: 64, StoredBytes: 5, ObjectBytes: 50},
	}
	got := FoldLedger(groups)

	want := LedgerStats{
		"a": {
			Objects:       63,
			StoredBytes:   110,
			Unhashed:      1 + 16,
			Plaintext:     1 + 2 + 4 + 16,
			Unreadable:    8,
			Compressed:    CompressionStat{Objects: 32, LogicalBytes: 400, StoredBytes: 100},
			Verifiable:    2 + 8 + 32,
			NeverVerified: 1 + 2,
		},
		"b": {
			Objects:     64,
			StoredBytes: 5,
			Unhashed:    64,
			Plaintext:   64,
			Compressed:  CompressionStat{Objects: 64, LogicalBytes: 50, StoredBytes: 5},
		},
	}
	if !maps.Equal(got, want) {
		t.Errorf("FoldLedger =\n%+v\nwant\n%+v", got, want)
	}
}

// TestFoldLedger_Empty verifies no groups fold to no backends.
func TestFoldLedger_Empty(t *testing.T) {
	t.Parallel()
	if got := FoldLedger(nil); len(got) != 0 {
		t.Errorf("FoldLedger(nil) = %v, want empty", got)
	}
}
