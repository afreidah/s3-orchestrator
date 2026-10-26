// -------------------------------------------------------------------------------
// Copy Sets - Objects Off the Replication Factor
//
// Author: Alex Freidah
//
// The replicator and the over-replication cleaner do the same pass from
// opposite sides: group an object's copies, keep the objects whose copy count
// is off the factor, and work through each one under admission while counting
// the copies changed. This file is that pass; each worker supplies which side
// of the factor it acts on and what it does to one object.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// copySet is one object whose copy count is off the replication factor: its
// copies, and how many copies it is short or over by.
type copySet struct {
	key    string
	copies []core.ObjectLocation
	off    int
}

// planCopySets groups location rows by key and keeps each key that offBy
// reports a positive count for, given how many copies it has. The result order
// is not stable.
func planCopySets(locations []core.ObjectLocation, offBy func(copies int) int) []copySet {
	grouped := core.GroupByKey(locations)
	sets := make([]copySet, 0, len(grouped))
	for key, copies := range grouped {
		if off := offBy(len(copies)); off > 0 {
			sets = append(sets, copySet{key: key, copies: copies, off: off})
		}
	}
	return sets
}

// shortBy counts the copies a key needs to reach factor.
func shortBy(factor int) func(int) int {
	return func(copies int) int { return factor - copies }
}

// overBy counts the copies a key holds beyond factor.
func overBy(factor int) func(int) int {
	return func(copies int) int { return copies - factor }
}

// copySetPass is how a pass works through its copy sets: its log, how many
// objects it works on at once, and who watches.
type copySetPass struct {
	log         *slog.Logger
	concurrency int
	observer    progress.Observer
}

// run works through sets, each under an admission slot, and returns the tally
// with the number of copies fn reports it changed across every set.
func (p copySetPass) run(ctx context.Context, ops Ops, sets []copySet, fn func(ctx context.Context, set *copySet) (int, batch.ItemResult)) (batch.Summary, int) {
	var changed atomic.Int64
	runner := batch.Runner[copySet]{
		Log:         p.log,
		Concurrency: p.concurrency,
		Observer:    p.observer,
		Key:         func(s copySet) string { return s.key },
	}
	sum := runner.Run(ctx, sets, func(ctx context.Context, set copySet) batch.ItemResult {
		return WithAdmission(ctx, ops, func() batch.ItemResult {
			n, res := fn(ctx, &set)
			changed.Add(int64(n))
			return res
		})
	})
	return sum, int(changed.Load())
}
