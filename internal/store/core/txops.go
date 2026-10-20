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

import (
	"context"
	"time"
)

// TxOps holds the Runner that the shared methods run their transactions
// against. It is embedded by each engine store, which is itself that Runner.
type TxOps struct {
	runner Runner
}

// NewTxOps binds the shared methods to the store that will embed them.
func NewTxOps(r Runner) TxOps { return TxOps{runner: r} }

// -------------------------------------------------------------------------
// SHARED TRANSACTIONAL OPERATIONS
// -------------------------------------------------------------------------

// RecordObject records an object's copies and tag set in one transaction,
// returning the cleanup rows it queued and the byte deltas it charged.
func (o TxOps) RecordObject(ctx context.Context, req *RecordObjectRequest) ([]CleanupItem, QuotaDeltas, error) {
	return RecordObject(ctx, o.runner, req)
}

// DeleteObject removes every copy of an object, returning the cleanup rows it
// queued for their bytes and the byte deltas their removal made.
func (o TxOps) DeleteObject(ctx context.Context, key string) ([]CleanupItem, QuotaDeltas, error) {
	return DeleteObject(ctx, o.runner, key)
}

// DeleteObjectsBatch removes every supplied key in one transaction and returns
// the cleanup rows it queued, plus the deltas folded per backend.
func (o TxOps) DeleteObjectsBatch(ctx context.Context, keys []string) ([]CleanupItem, QuotaDeltas, error) {
	return DeleteObjectsBatch(ctx, o.runner, keys)
}

// DeleteObjectLocation removes the copy loc names while it is still the copy
// the caller read, queueing its bytes under reason unless reason is empty.
func (o TxOps) DeleteObjectLocation(ctx context.Context, loc *ObjectLocation, keepAtLeast int, reason string) ([]CleanupItem, error) {
	return DeleteObjectLocation(ctx, o.runner, loc, keepAtLeast, reason)
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
func (o TxOps) ImportObject(ctx context.Context, req *ImportObjectRequest) (ImportOutcome, error) {
	return ImportObject(ctx, o.runner, req)
}

// PlaceCopy records a copy at the path its intent claimed, removing the source
// for a move, or queues the copied bytes when the placement is refused.
func (o TxOps) PlaceCopy(ctx context.Context, p *Placement) (int64, []CleanupItem, error) {
	return PlaceCopy(ctx, o.runner, p)
}

// DiscardPending drops an intent whose upload will not be recorded and queues
// its bytes.
func (o TxOps) DiscardPending(ctx context.Context, p *PendingObject, reason string) ([]CleanupItem, error) {
	return DiscardPending(ctx, o.runner, p, reason)
}

// RecordPart records an uploaded part and clears its intent, queueing the bytes
// of a part it replaced.
func (o TxOps) RecordPart(ctx context.Context, p *RecordPartParams) ([]CleanupItem, error) {
	return RecordPart(ctx, o.runner, p)
}

// DeleteMultipartUpload removes an upload and its parts and queues the parts'
// bytes, unless a completion renewed its mark after staleBefore.
func (o TxOps) DeleteMultipartUpload(ctx context.Context, uploadID, reason string, staleBefore time.Time) ([]CleanupItem, error) {
	return DeleteMultipartUpload(ctx, o.runner, uploadID, reason, staleBefore)
}

// InsertPendingIfFits admits an intent on its backend, serialized against the
// backend's other claims when it has a byte ceiling.
func (o TxOps) InsertPendingIfFits(ctx context.Context, p *PendingObject, accepts BackendFlags) (bool, error) {
	return InsertPendingIfFits(ctx, o.runner, p, accepts)
}

// PromotePending resolves a surviving PUT intent into a committed location.
func (o TxOps) PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []CleanupItem, QuotaDeltas, error) {
	return PromotePending(ctx, o.runner, p)
}

// CommitCompanionCopy adds an extra copy whose upload finished after the client
// was answered, or reports that a newer write took the key first.
func (o TxOps) CommitCompanionCopy(ctx context.Context, p *PendingObject) (CompanionCommitResult, []CleanupItem, QuotaDeltas, error) {
	return CommitCompanionCopy(ctx, o.runner, p)
}
