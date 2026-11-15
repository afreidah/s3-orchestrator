// -------------------------------------------------------------------------------
// Copy Sets - Objects Off the Replication Factor
//
// Author: Alex Freidah
//
// The replicator and the over-replication cleaner do the same pass from
// opposite sides: read the queued keys on their side of the factor, and work
// through each one under admission while counting the copies changed, then
// settle it or hold it for a retry. This file is that pass; each worker
// supplies which side it acts on and what it does to one object.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// copySet is one queued object whose copy count is off the replication factor:
// its copies, how many copies it is short or over by, and its queue entry.
type copySet struct {
	key    string
	copies []core.ObjectLocation
	off    int
	queued *core.QueuedReplication
}

// copySetPass is how a pass works through its copy sets: the queue it settles
// them in, how many objects it works on at once, and who watches.
type copySetPass struct {
	queue       *ReplicationQueue
	concurrency int
	observer    progress.Observer
}

// run works through sets, each under an admission slot, and returns the tally
// with the number of copies fn reports it changed across every set. fn also
// reports whether the set reached the factor, which settles its key; one that
// did not is held for a retry. A set the pass never reached stays due, and the
// first key it failed to settle or retry is its error.
func (p copySetPass) run(ctx context.Context, ops Ops, sets []copySet, fn func(ctx context.Context, set *copySet) (int, bool, batch.ItemResult)) (batch.Summary, int, error) {
	var (
		changed  atomic.Int64
		failOnce sync.Once
		failed   error
	)
	runner := batch.Runner[copySet]{
		Concurrency: p.concurrency,
		Admission:   ops,
		Observer:    p.observer,
		Key:         func(s copySet) string { return s.key },
	}
	sum := runner.Run(ctx, sets, func(ctx context.Context, set copySet) batch.ItemResult {
		n, done, res := fn(ctx, &set)
		changed.Add(int64(n))
		if err := p.queue.finish(ctx, set.queued, done); err != nil {
			failOnce.Do(func() { failed = err })
		}
		return res
	})
	return sum, int(changed.Load()), failed
}
