// -------------------------------------------------------------------------------
// Ledger Stats - Per-Backend Figures From the Ledger's Counters
//
// Author: Alex Freidah
//
// The object counts, the checksum backlog, the plaintext and unreadable copy
// counts, what compression saves, and how far behind verification is all come
// from counters every transaction that changes a copy charges, so reading them
// costs a few rows rather than a pass over object_locations. The figures are
// kept per backend so a consumer can sum them, read them per backend, or split
// them by which backends are reachable, as coverage does.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"slices"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LedgerStat is one backend's share of the ledger. Objects counts every row,
// managed or not, and StoredBytes what they hold. Unhashed counts managed rows
// with no content hash, the checksum backfill's backlog, and UnmanagedUnhashed
// the unmanaged ones, which backfill leaves alone. Plaintext counts rows stored
// unencrypted, the set encrypt-existing would process, and Unreadable counts
// rows encrypted with no key. Compressed covers encoded copies only. Verifiable
// counts the hashed managed rows the scrub sweep reads, and NeverVerified those
// of them never scrubbed.
type LedgerStat struct {
	Objects           int64
	StoredBytes       int64
	Unhashed          int64
	UnmanagedUnhashed int64
	Plaintext         int64
	Unreadable        int64
	Compressed        CompressionStat
	Verifiable        int64
	NeverVerified     int64
}

// LedgerStats is the ledger's figures keyed by backend name. A backend that
// holds no rows is absent.
type LedgerStats map[string]LedgerStat

// LedgerCounts is what a set of copies holds: how many there are, their stored
// and client-facing bytes, and how many were never scrubbed.
type LedgerCounts struct {
	Copies        int64
	StoredBytes   int64
	ObjectBytes   int64
	NeverScrubbed int64
}

// ScrubQueueReader reads the copies at the head of the scrub queue. Both
// engines satisfy it.
type ScrubQueueReader interface {
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]ObjectLocation, error)
}

// LedgerGroup is the ledger's copies on one backend that share a stored form
// and a managed flag, and what they hold. The counters keep one per stripe,
// both engines read them summed over the stripes, and FoldLedger turns them
// into figures, so what each figure counts is stated once.
type LedgerGroup struct {
	Backend string
	Form    StoredFlags
	Managed bool
	LedgerCounts
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FoldLedger turns the grouped ledger into per-backend figures, summing every
// group a backend holds.
func FoldLedger(groups []LedgerGroup) LedgerStats {
	out := make(LedgerStats)
	for i := range groups {
		g := &groups[i]
		st := out[g.Backend]
		st.add(g)
		out[g.Backend] = st
	}
	return out
}

// UnhashedCount returns how many copies on backend, or on every backend when it
// is empty, have no content hash, counting only the managed or only the
// unmanaged ones when managed is set. It totals what UnhashedCopies selects.
func (s LedgerStats) UnhashedCount(backend string, managed *bool) int64 {
	var n int64
	for name, st := range s {
		if backend != "" && name != backend {
			continue
		}
		if managed == nil || *managed {
			n += st.Unhashed
		}
		if managed == nil || !*managed {
			n += st.UnmanagedUnhashed
		}
	}
	return n
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

// Coverage reports how far behind verification is as of now, split by whether
// the sweep can reach each backend. oldest is the earliest last scrub, or write
// for a copy never scrubbed, among the verifiable copies on reachable backends,
// and zero when they hold none. The age and the never-verified count cover
// reachable backends only: a copy the sweep may not read can never be stamped,
// so counting it would pin the age to a fixed timestamp and grow it by a day
// every day. Deferred counts the verifiable copies on the rest, so a fleet
// mostly over its usage limit does not read as healthy.
func (s LedgerStats) Coverage(reachable []string, oldest, now time.Time) CoverageStat {
	var cov CoverageStat
	for name, st := range s {
		if !slices.Contains(reachable, name) {
			cov.Deferred += st.Verifiable
			continue
		}
		cov.NeverVerified += st.NeverVerified
	}
	if !oldest.IsZero() && now.After(oldest) {
		cov.OldestUnverifiedAge = now.Sub(oldest)
	}
	return cov
}

// OldestTouched returns when the copy at the head of the scrub queue on
// backends was last touched, the oldest among their verifiable copies, or
// zero when they hold none.
func OldestTouched(ctx context.Context, r ScrubQueueReader, backends []string, now time.Time) (time.Time, error) {
	head, err := r.GetLeastRecentlyScrubbedObjects(ctx, 1, backends, now)
	if err != nil || len(head) == 0 {
		return time.Time{}, err
	}
	return head[0].LastTouched(), nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// add counts one ledger group into the backend's figures. Verification counts
// hashed managed rows, the population the scrub queue draws from.
func (st *LedgerStat) add(g *LedgerGroup) {
	st.Objects += g.Copies
	st.StoredBytes += g.StoredBytes
	if !g.Form.Has(StoredHashed) {
		if g.Managed {
			st.Unhashed += g.Copies
		} else {
			st.UnmanagedUnhashed += g.Copies
		}
	}
	if !g.Form.Has(StoredEncrypted) {
		st.Plaintext += g.Copies
	}
	if g.Form.Unreadable() {
		st.Unreadable += g.Copies
	}
	if g.Form.Has(StoredCompressed) {
		st.Compressed.Objects += g.Copies
		st.Compressed.LogicalBytes += g.ObjectBytes
		st.Compressed.StoredBytes += g.StoredBytes
	}
	if !g.Managed || !g.Form.Has(StoredHashed) {
		return
	}
	st.Verifiable += g.Copies
	st.NeverVerified += g.NeverScrubbed
}

// copyGroup is the group one copy counts in, holding that copy alone. Every
// row starts never scrubbed, since nothing stamps a scrub on insert.
func copyGroup(backend string, form StoredFlags, unmanaged bool, stored, object int64, scrubbed bool) LedgerGroup {
	counts := LedgerCounts{Copies: 1, StoredBytes: stored, ObjectBytes: object}
	if !scrubbed {
		counts.NeverScrubbed = 1
	}
	return LedgerGroup{Backend: backend, Form: form, Managed: !unmanaged, LedgerCounts: counts}
}

// insertedGroup is the group the row inserting l counts in: its recorded form,
// which drops a stray key or encoding field, and never scrubbed.
func insertedGroup(l *ObjectLocation) LedgerGroup {
	form := l.Recorded()
	return copyGroup(l.BackendName, form.Form(), l.Unmanaged, l.SizeBytes, form.ClientSize(l.SizeBytes), false)
}

// group is the group the copy counts in.
func (c *ExistingCopy) group() LedgerGroup {
	return copyGroup(c.BackendName, c.Form, c.Unmanaged, c.SizeBytes, c.ObjectSize, c.Scrubbed)
}

// plus returns the counts with o's times n added, so n of -1 takes them off.
func (c LedgerCounts) plus(o LedgerCounts, n int64) LedgerCounts {
	return LedgerCounts{
		Copies:        c.Copies + n*o.Copies,
		StoredBytes:   c.StoredBytes + n*o.StoredBytes,
		ObjectBytes:   c.ObjectBytes + n*o.ObjectBytes,
		NeverScrubbed: c.NeverScrubbed + n*o.NeverScrubbed,
	}
}

// total sums one figure across every backend.
func (s LedgerStats) total(field func(LedgerStat) int64) int64 {
	var n int64
	for _, st := range s {
		n += field(st)
	}
	return n
}
