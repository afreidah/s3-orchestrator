// -------------------------------------------------------------------------------
// Replication Queue - The Keys Off the Factor, as the Workers Drain Them
//
// Author: Alex Freidah
//
// The replicator and the over-replication cleaner work through the replication
// queue instead of scanning the ledger: every write that leaves a key off the
// factor queues it in its own transaction, so a pass reads a batch of due keys,
// brings each back to its factor, and settles it or holds it for a later retry.
// Passes run one at a time under the replicator's lock, so a pass that stops
// partway leaves its unfinished keys due as they were. Both sides share one
// ReplicationQueue, which rebuilds the queue from the ledger when it was not
// kept against what a pass needs: on the first pass, after the factors, the
// excluded backends or the fleet changed, and on any pass at factors the
// store's writes are not queueing against.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/backoff"
)

// replicationRetry spaces the retries of a key a pass could not bring back to
// the factor, such as one with no target that has room.
var replicationRetry = backoff.Exponential{Base: time.Minute, Ceiling: time.Hour}

// ReplicationQueueStore is the persistence surface the queue needs: the
// replication queue itself and the copies of each key it hands out.
type ReplicationQueueStore interface {
	core.ReplicationStore
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
}

// ReplicationQueue is the replication queue as the workers drain it. basis
// names the factor, excluded backends and fleet the last rebuild was against.
type ReplicationQueue struct {
	store ReplicationQueueStore
	log   *slog.Logger
	mu    sync.Mutex
	basis string
}

// NewReplicationQueue builds the queue the replicator and the cleaner share
// over store.
func NewReplicationQueue(store ReplicationQueueStore) *ReplicationQueue {
	return &ReplicationQueue{store: store, log: logfmt.Logger(string(audit.WorkloadReplication))}
}

// Factors returns the factors a pass at the deployment's factor brings keys
// to: that one, and each bucket's own as the store's writes queue them.
func (q *ReplicationQueue) Factors(deployment int) core.ReplicationFactors {
	factors := q.store.ReplicationFactors()
	factors.Default = deployment
	return factors
}

// prepare rebuilds the queue unless the store's writes queue keys against
// factors and nothing it was last rebuilt against has changed.
func (q *ReplicationQueue) prepare(ctx context.Context, factors *core.ReplicationFactors, excluded []string, backends map[string]backend.ObjectBackend) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	basis := basisOf(factors, excluded, backends)
	kept := q.store.ReplicationFactors()
	if kept.Equal(factors) && basis == q.basis {
		return nil
	}
	_, err := q.rebuildLocked(ctx, factors, excluded, basis)
	return err
}

// rebuild queues every key the ledger holds off its factor, whatever the queue
// was last rebuilt against, and reports how many it queued.
func (q *ReplicationQueue) rebuild(ctx context.Context, factors *core.ReplicationFactors, excluded []string, backends map[string]backend.ObjectBackend) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.rebuildLocked(ctx, factors, excluded, basisOf(factors, excluded, backends))
}

// rebuildLocked rebuilds the queue and records basis as what it was rebuilt
// against. The caller holds mu.
func (q *ReplicationQueue) rebuildLocked(ctx context.Context, factors *core.ReplicationFactors, excluded []string, basis string) (int64, error) {
	queued, err := q.store.RebuildReplicationQueue(ctx, factors, excluded)
	if err != nil {
		return 0, fmt.Errorf("rebuild replication queue: %w", err)
	}
	q.basis = basis
	audit.Log(ctx, "replication.queue_rebuilt", slog.String("factors", factors.String()), slog.Int64("queued", queued))
	return queued, nil
}

// basisOf names the factors, excluded backends and fleet a rebuild counts
// against, so a change to any of them shows as a different basis.
func basisOf(factors *core.ReplicationFactors, excluded []string, backends map[string]backend.ObjectBackend) string {
	return fmt.Sprintf("%s|%s|%s", factors, strings.Join(slices.Sorted(slices.Values(excluded)), ","),
		strings.Join(slices.Sorted(maps.Keys(backends)), ","))
}

// read reads a side's due keys and returns the ones off by more than zero, as
// offBy counts them, with their copies. A key already back at the factor, or
// gone, is settled here.
func (q *ReplicationQueue) read(ctx context.Context, side core.ReplicationDirection, limit int, excluded []string, offBy func(*core.QueuedReplication) int) ([]copySet, error) {
	due, err := q.store.ListReplicationQueue(ctx, side, limit, excluded)
	if err != nil {
		return nil, fmt.Errorf("read replication queue: %w", err)
	}
	sets := make([]copySet, 0, len(due))
	for i := range due {
		queued := &due[i]
		set, done := q.copySetOf(ctx, queued, offBy(queued))
		if set != nil {
			sets = append(sets, *set)
			continue
		}
		if err := q.finish(ctx, queued, done); err != nil {
			return nil, err
		}
	}
	return sets, nil
}

// copySetOf builds the copy set of a key off by off, or reports whether one
// that needs no work is done: a key at the factor or gone is, and one whose
// copies could not be read is held for a retry. A key with no copy recorded
// yet is not gone while it holds a live intent, since Live counts it beyond
// the copies Held does, so it is held until that upload is promoted or
// dropped; settling it would lose the key, because the promotion leaves its
// count unchanged and queues nothing.
func (q *ReplicationQueue) copySetOf(ctx context.Context, queued *core.QueuedReplication, off int) (*copySet, bool) {
	if off <= 0 {
		return nil, true
	}
	copies, err := core.ClientLocations(q.store.GetAllObjectLocations(ctx, queued.Key))
	switch {
	case errors.Is(err, core.ErrObjectNotFound):
		return nil, queued.Live <= queued.Held
	case err != nil:
		q.log.WarnContext(ctx, "failed to read a queued key's copies", "key", queued.Key, "error", err)
		return nil, false
	}
	return &copySet{key: queued.Key, copies: copies, off: off, queued: queued}, false
}

// finish settles a key a pass brought back to the factor, or holds it for a
// retry after a backoff that grows with its attempts. A failed write leaves the
// key due, so the next pass reads it again.
func (q *ReplicationQueue) finish(ctx context.Context, queued *core.QueuedReplication, done bool) error {
	var err error
	if done {
		err = q.store.SettleReplicationQueue(ctx, queued)
	} else {
		err = q.store.RetryReplicationQueue(ctx, queued, time.Now().Add(replicationRetry.For(queued.Attempts)))
	}
	if err != nil {
		return fmt.Errorf("release queued key %s: %w", queued.Key, err)
	}
	return nil
}
