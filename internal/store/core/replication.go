// -------------------------------------------------------------------------------
// Core Replication Orchestration
//
// Author: Alex Freidah
//
// Types for replica management. A key off the replication factor is held in
// the replication queue until a drain brings it back: the ledger edit queues
// the keys its transaction left off the factor, and a rebuild queues every one
// the ledger holds. A replica is recorded by PlaceCopy, and surplus copies are
// removed by DeleteObjectLocation with keepAtLeast set.
// -------------------------------------------------------------------------------

package core

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/internalkey"
)

// ReplicationFactors is how many copies each key is kept at: Default, or the
// factor its bucket sets in Buckets. Buckets is shared between the copies a
// snapshot is handed to and must not be modified.
type ReplicationFactors struct {
	Default int
	Buckets map[string]int
}

// For returns the factor key is kept at.
func (f *ReplicationFactors) For(key string) int {
	bucket, _ := internalkey.Split(key)
	if n, ok := f.Buckets[bucket]; ok {
		return n
	}
	return f.Default
}

// Replicates reports whether the replication workers have anything to keep:
// a default above one, or a bucket setting a factor of its own, which may be
// one lower than the copies it holds.
func (f *ReplicationFactors) Replicates() bool {
	return f.Default > 1 || len(f.Buckets) > 0
}

// Overrides lists the buckets setting a factor of their own and those
// factors, in name order, as a query binds them.
func (f *ReplicationFactors) Overrides() ([]string, []int64) {
	names := slices.Sorted(maps.Keys(f.Buckets))
	factors := make([]int64, len(names))
	for i, name := range names {
		factors[i] = int64(f.Buckets[name])
	}
	return names, factors
}

// String renders the factors so two equal sets render alike, which is what
// tells a queue rebuilt against them that they changed.
func (f *ReplicationFactors) String() string {
	names, factors := f.Overrides()
	var b strings.Builder
	fmt.Fprintf(&b, "%d", f.Default)
	for i, name := range names {
		fmt.Fprintf(&b, ",%s=%d", name, factors[i])
	}
	return b.String()
}

// Equal reports whether f and o keep every key at the same factor.
func (f *ReplicationFactors) Equal(o *ReplicationFactors) bool {
	return f.Default == o.Default && maps.Equal(f.Buckets, o.Buckets)
}

// FactorsHolder holds where replication factors are read from, which a store
// is told once wiring settles them. The zero value reads none.
type FactorsHolder struct {
	source atomic.Pointer[func() ReplicationFactors]
}

// Use names where Load reads the factors from, so each read gets the running
// values.
func (h *FactorsHolder) Use(factors func() ReplicationFactors) {
	h.source.Store(&factors)
}

// Load returns the factors as they stand now, and none before Use names a
// source.
func (h *FactorsHolder) Load() ReplicationFactors {
	if f := h.source.Load(); f != nil {
		return (*f)()
	}
	return ReplicationFactors{}
}

// ReplicationDirection says which side of the factor a queued key is on.
type ReplicationDirection int16

// A queued key is short of the factor or past it.
const (
	ReplicationShort ReplicationDirection = -1
	ReplicationOver  ReplicationDirection = 1
)

// directionOf returns the side of factor that count is on.
func directionOf(count, factor int) ReplicationDirection {
	if count < factor {
		return ReplicationShort
	}
	return ReplicationOver
}

// ReplicationBacklog counts the queued keys on each side of the factor.
type ReplicationBacklog struct {
	Under int64
	Over  int64
}

// QueuedReplication is a key the replication queue holds, as a drain read it.
// Live counts its copies outside the excluded backends plus its live intents,
// the copies it will have once every upload lands; Held counts every managed
// copy recorded. Version is what a settle or retry must match.
type QueuedReplication struct {
	Key       string
	Direction ReplicationDirection
	Attempts  int32
	Version   int64
	Live      int
	Held      int
}
