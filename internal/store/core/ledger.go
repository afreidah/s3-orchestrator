// -------------------------------------------------------------------------------
// Ledger Stats - Per-Backend Figures From One Pass Over the Ledger
//
// Author: Alex Freidah
//
// The object counts, the checksum backlog, the plaintext and unreadable copy
// counts, what compression saves, and how far behind verification is all come
// from one grouped pass over object_locations rather than one full scan each.
// The figures are kept per backend so a consumer can sum them, read them per
// backend, or split them by which backends are reachable, as coverage does.
// -------------------------------------------------------------------------------

package core

import "time"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LedgerStat is one backend's share of the ledger. Objects counts every row,
// managed or not. Unhashed counts rows with no content hash, the checksum
// backfill's backlog. Plaintext counts rows stored unencrypted, the set
// encrypt-existing would process, and Unreadable counts rows encrypted with no
// key. Compressed covers encoded copies only. Verifiable counts the hashed
// managed rows the scrub sweep reads, NeverVerified those of them never
// scrubbed, and OldestTouched is the earliest of their last scrub or, for one
// never scrubbed, their write; it is zero when Verifiable is.
type LedgerStat struct {
	Objects       int64
	Unhashed      int64
	Plaintext     int64
	Unreadable    int64
	Compressed    CompressionStat
	Verifiable    int64
	NeverVerified int64
	OldestTouched time.Time
}

// LedgerStats is the ledger's figures keyed by backend name. A backend that
// holds no rows is absent.
type LedgerStats map[string]LedgerStat

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ObjectCounts returns each backend's row count.
func (s LedgerStats) ObjectCounts() map[string]int64 {
	return s.perBackend(func(st LedgerStat) int64 { return st.Objects })
}

// UnhashedCounts returns each backend's count of rows with no content hash.
func (s LedgerStats) UnhashedCounts() map[string]int64 {
	return s.perBackend(func(st LedgerStat) int64 { return st.Unhashed })
}

// PlaintextCopies returns how many copies across the fleet are stored
// unencrypted.
func (s LedgerStats) PlaintextCopies() int64 {
	return s.total(func(st LedgerStat) int64 { return st.Plaintext })
}

// UnreadableCopies returns how many copies across the fleet are encrypted with
// no key.
func (s LedgerStats) UnreadableCopies() int64 {
	return s.total(func(st LedgerStat) int64 { return st.Unreadable })
}

// Compression returns each backend's encoded-copy totals. Backends holding no
// encoded copies are absent rather than present as zeroes.
func (s LedgerStats) Compression() map[string]CompressionStat {
	out := make(map[string]CompressionStat)
	for name, st := range s {
		if st.Compressed.Objects > 0 {
			out[name] = st.Compressed
		}
	}
	return out
}

// Coverage reports how far behind verification is as of now, split by whether
// the sweep can reach each backend. The age and the never-verified count cover
// reachable backends only: a copy the sweep may not read can never be stamped,
// so counting it would pin the age to a fixed timestamp and grow it by a day
// every day. Deferred counts the verifiable copies on the rest, so a fleet
// mostly over its usage limit does not read as healthy.
func (s LedgerStats) Coverage(reachable []string, now time.Time) CoverageStat {
	canReach := make(map[string]bool, len(reachable))
	for _, name := range reachable {
		canReach[name] = true
	}
	var cov CoverageStat
	var oldest time.Time
	for name, st := range s {
		if !canReach[name] {
			cov.Deferred += st.Verifiable
			continue
		}
		cov.NeverVerified += st.NeverVerified
		if st.Verifiable > 0 && (oldest.IsZero() || st.OldestTouched.Before(oldest)) {
			oldest = st.OldestTouched
		}
	}
	if !oldest.IsZero() && now.After(oldest) {
		cov.OldestUnverifiedAge = now.Sub(oldest)
	}
	return cov
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// perBackend maps each backend to one of its figures.
func (s LedgerStats) perBackend(field func(LedgerStat) int64) map[string]int64 {
	out := make(map[string]int64, len(s))
	for name, st := range s {
		out[name] = field(st)
	}
	return out
}

// total sums one figure across every backend.
func (s LedgerStats) total(field func(LedgerStat) int64) int64 {
	var n int64
	for _, st := range s {
		n += field(st)
	}
	return n
}
