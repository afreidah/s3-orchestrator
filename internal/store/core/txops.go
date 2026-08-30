// -------------------------------------------------------------------------------
// Shared Transactional Store Methods
//
// Author: Alex Freidah
//
// The store methods whose whole implementation is a transaction composed in
// this package. The engines contribute nothing to them beyond being the
// Runner that supplies the TxAdapter, so they are defined once here and
// embedded by each engine store rather than restated per engine.
//
// An engine that needs to diverge declares the method on its own Store, which
// shadows the promoted one.
// -------------------------------------------------------------------------------

package core

import "context"

// TxOps holds the Runner that the shared methods run their transactions
// against. It is embedded by each engine store, which is itself that Runner.
type TxOps struct {
	runner Runner
}

// NewTxOps binds the shared methods to the store that will embed them.
func NewTxOps(r Runner) TxOps { return TxOps{runner: r} }

// RecordObject records an object's location, its tag set and the backend
// quota in one transaction, removing and returning any copies the write
// displaces. A non-empty IntentID also clears the matching pending intent.
func (o TxOps) RecordObject(ctx context.Context, req *RecordObjectRequest) ([]DeletedCopy, error) {
	return RecordObject(ctx, o.runner, req)
}

// DeleteObject removes every copy of an object and decrements the quotas,
// returning the deleted copies for backend cleanup.
func (o TxOps) DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error) {
	return DeleteObject(ctx, o.runner, key)
}

// DeleteObjectsBatch removes every supplied key in one transaction and
// returns the displaced copies per key.
func (o TxOps) DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]DeletedCopy, error) {
	return DeleteObjectsBatch(ctx, o.runner, keys)
}

// DeleteObjectLocation removes a single copy and decrements that backend's
// quota.
func (o TxOps) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	return DeleteObjectLocation(ctx, o.runner, key, backendName)
}

// ReplaceObjectTags swaps an object's whole tag set for the supplied one.
func (o TxOps) ReplaceObjectTags(ctx context.Context, key string, tags []Tag) error {
	return ReplaceObjectTags(ctx, o.runner, key, tags)
}

// DeleteObjectTags removes an object's whole tag set.
func (o TxOps) DeleteObjectTags(ctx context.Context, key string) error {
	return DeleteObjectTags(ctx, o.runner, key)
}

// ImportObject records bytes discovered on a backend, leaving an existing row
// untouched.
func (o TxOps) ImportObject(ctx context.Context, key, backend string, size int64, unmanaged bool, form *StoredForm) (ImportOutcome, error) {
	return ImportObject(ctx, o.runner, key, backend, size, unmanaged, form)
}

// MoveObjectLocation repoints a copy at a different backend and moves the
// bytes between the two quotas.
func (o TxOps) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return MoveObjectLocation(ctx, o.runner, key, fromBackend, toBackend)
}

// SweepStaleCleanupQueueRows drops queued cleanups for a key that has been
// rewritten on the same backend.
func (o TxOps) SweepStaleCleanupQueueRows(ctx context.Context, key, backend string) (int64, error) {
	return SweepStaleCleanupQueueRows(ctx, o.runner, key, backend)
}

// MoveCleanupToDLQ graduates an exhausted cleanup row to the dead-letter
// queue for operator triage.
func (o TxOps) MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error) {
	return MoveCleanupToDLQ(ctx, o.runner, id, lastError)
}

// RecordReplica records a new copy on a target backend and charges its quota.
func (o TxOps) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (int64, bool, error) {
	return RecordReplica(ctx, o.runner, key, targetBackend, sourceBackend)
}

// RemoveExcessCopy drops one copy of an over-replicated object, refusing the
// copy that would leave the object unreadable.
func (o TxOps) RemoveExcessCopy(ctx context.Context, key, backendName string, factor int) (bool, error) {
	return RemoveExcessCopy(ctx, o.runner, key, backendName, factor)
}

// ReconcileUsage recomputes bytes_used per backend from the location rows,
// correcting drift in the incrementally maintained counter.
func (o TxOps) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	return ReconcileUsage(ctx, o.runner)
}

// PromotePending resolves a surviving PUT intent into a committed location.
func (o TxOps) PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []DeletedCopy, error) {
	return PromotePending(ctx, o.runner, p)
}
