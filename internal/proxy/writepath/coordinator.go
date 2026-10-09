// -------------------------------------------------------------------------------
// Write Coordinator - Shared Write-Path Helpers
//
// Author: Alex Freidah
//
// Owns the helpers that combine the per-role store views with the backend
// runtime primitives to record objects, promote pending intents, enqueue
// cleanups, and pick write targets. The object and multipart managers hold a
// *Coordinator directly, so each is fully initialised at construction time
// without post-construction patching.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

// Coordinator bundles the infrastructure subset (WriteRuntime) with
// the metadata-store contract and the pending-pattern flag so the
// write-path helpers can be expressed as plain methods on one value every
// consumer shares, with no post-construction wiring step.
// CoordinatorStores is the narrow persistence surface the write coordinator needs:
// object record/move, write-target selection (quota), pending-intent
// insert/promote, and cleanup enqueue/recovery. Declared locally so
// writepath does not pull in the full MetadataStore.
//go:generate mockgen -destination=mock_stores_test.go -package=writepath github.com/afreidah/s3-orchestrator/internal/proxy/writepath CoordinatorStores

type CoordinatorStores interface {
	core.ObjectStore
	core.QuotaStore
	core.PendingStore
	core.CleanupStore
	CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error)
}

type Coordinator struct {
	core   WriteRuntime // infrastructure subset: backends, usage, routing, eligibility, error classification, delete-with-timeout
	stores CoordinatorStores
	log    *slog.Logger
}

// New constructs a Coordinator. The supplied core must observe the same
// admission, usage, drain, and backend state every other collaborator sees;
// in production they are all handed the one *infra.BackendRuntime. The
// component-scoped logger is built in the constructor body per the
// project's logging convention.
func New(core WriteRuntime, stores CoordinatorStores) *Coordinator {
	must.NotNil("core", core)
	must.NotNil("stores", stores)
	return &Coordinator{
		core:   core,
		stores: stores,
		log:    slog.Default().With(logfmt.Component("writepath")),
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// ClaimWriteTarget picks the target backend for a write using the configured
// routing strategy and claims the bytes on it with the write's intent. "pack"
// takes the first eligible backend with room, "spread" the least utilized one.
// Returns ErrNoSpaceAvailable when no candidate has room.
func (w *Coordinator) ClaimWriteTarget(ctx context.Context, p *core.PendingObject, eligible []string) (string, error) {
	name, err := w.claimFirst(eligible, func(name string) (bool, error) {
		p.BackendName = name
		return w.stores.InsertPendingIfFits(ctx, p)
	})
	if err != nil {
		return "", err
	}
	// Tell the ranking what this instance just placed, or every write in the
	// interval before the next reload ranks the candidates identically and
	// spread stops spreading.
	w.core.Quota().NotePlacement(name, p.SizeBytes)
	telemetry.PendingIntentsEnqueuedTotal.Inc()
	return name, nil
}

// claimFirst walks the eligible backends in routing order and returns the first
// whose conditional insert accepts.
//
// The order comes from a possibly stale snapshot; room is decided by the insert
// against live rows. A declined candidate is skipped. Returns
// ErrNoSpaceAvailable when none accepts, and stops on a database error.
func (w *Coordinator) claimFirst(eligible []string, try func(name string) (bool, error)) (string, error) {
	for _, name := range w.rankForWrite(w.core.Quota(), eligible) {
		ok, err := try(name)
		if err != nil {
			return "", fmt.Errorf("claim write target: %w", err)
		}
		if ok {
			return name, nil
		}
		telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(name).Inc()
	}
	return "", core.ErrNoSpaceAvailable
}

// ClaimWriteCopies claims a distinct backend for each intent, using the same
// conditional insert a single-copy write uses, and returns the intents that got
// one, in claim order, with BackendName set. Claiming fewer than asked is not an
// error, since the replicator makes up the rest; only claiming none fails. A
// database error after the first claim ends the loop rather than the write.
func (w *Coordinator) ClaimWriteCopies(ctx context.Context, intents []*core.PendingObject, eligible []string) ([]*core.PendingObject, error) {
	claimed := make([]*core.PendingObject, 0, len(intents))
	for _, name := range w.rankForWrite(w.core.Quota(), eligible) {
		if len(claimed) == len(intents) {
			break
		}
		p := intents[len(claimed)]
		p.BackendName = name
		fits, err := w.stores.InsertPendingIfFits(ctx, p)
		if err != nil {
			if len(claimed) == 0 {
				return nil, fmt.Errorf("claim write target: %w", err)
			}
			w.log.WarnContext(ctx, "claim failed for a further copy, writing the copies already claimed",
				"key", p.ObjectKey, "backend", name, logfmt.Err(err))
			break
		}
		if !fits {
			telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(name).Inc()
			continue
		}
		w.core.Quota().NotePlacement(name, p.SizeBytes)
		telemetry.PendingIntentsEnqueuedTotal.Inc()
		claimed = append(claimed, p)
	}
	if len(claimed) == 0 {
		return nil, core.ErrNoSpaceAvailable
	}
	return claimed, nil
}

// rankForWrite orders the candidates the way the configured strategy wants them
// tried: pack keeps the configured order so writes fill one backend before
// moving on, spread puts the least utilized first. The slice is copied before
// sorting so the caller's eligibility list is left alone.
func (w *Coordinator) rankForWrite(quota *counter.QuotaTracker, eligible []string) []string {
	if w.core.RoutingStrategy() != config.RoutingSpread {
		return eligible
	}
	return quota.RankByUtilization(eligible)
}

// SelectWriteTarget picks a backend for a write and claims it with the pending
// intent p. Returns ErrInsufficientStorage when no backend can accept the
// write, or the classified selection error. The caller owns the intent from
// here: the transaction that records the object clears it, and any path that
// gives up leaves it for the reaper.
func (w *Coordinator) SelectWriteTarget(ctx context.Context, span trace.Span, operation s3op.Operation, p *core.PendingObject) (string, error) {
	eligible := w.core.EligibleForWrite([]s3op.Operation{operation}, 0, p.SizeBytes)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.ClaimWriteTarget(ctx, p, eligible)
	if err != nil {
		return "", w.core.ClassifyWriteError(span, operation.String(), err)
	}
	return name, nil
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// RecordObjectOrCleanup calls RecordObject and, on failure, deletes the
// orphaned bytes from be. On success it cleans up any copies the overwrite
// displaced. The transaction itself charges the bytes, so no counter is
// settled here. req must name exactly one copy, the one on be.
func (w *Coordinator) RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, req *core.RecordObjectRequest) error {
	backendName, err := soleBackend(req)
	if err != nil {
		observe.RecordSpanError(span, err)
		return err
	}
	displaced, _, err := w.stores.RecordObject(ctx, req)
	if err != nil {
		w.log.ErrorContext(ctx, "recordObject failed, cleaning up orphan",
			"key", req.Key, "backend", backendName, "error", err)
		w.RecoverFromRecordFailure(ctx, be, &core.CleanupRequest{
			BackendName: backendName,
			ObjectKey:   req.Key,
			StorageKey:  req.Copies[0].StorageKey,
			Reason:      "orphan_record_failed",
			SizeBytes:   req.Size,
		})
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}
	w.cleanupDisplacedCopies(ctx, req.Key, backendName, displaced)
	return nil
}

// soleBackend names the single copy a request places, or reports why it cannot.
// The helpers that clean up after a failed commit delete from one backend, so a
// request naming any other number is a caller error rather than a state they
// can recover from.
func soleBackend(req *core.RecordObjectRequest) (string, error) {
	if len(req.Copies) != 1 {
		return "", fmt.Errorf("%w: record request for %s names %d copies", errSingleCopyOnly, req.Key, len(req.Copies))
	}
	return req.Copies[0].Backend, nil
}

// errSingleCopyOnly reports a multi-copy request handed to a helper whose
// recovery path can only account for one.
var errSingleCopyOnly = errors.New("write path: helper records a single copy")

// reasonOverwriteDisplaced labels the cleanup of a copy an overwrite replaced,
// which is what the store reports when it does not say otherwise.
const reasonOverwriteDisplaced = "overwrite_displaced"

// RecoverFromRecordFailure deletes the bytes a write uploaded after its commit
// failed, charging both the PUT and the cleanup DELETE. A failed delete is
// enqueued for retry; a 404 counts as done. It deletes only c's storage key,
// which no other write shares. Callers own the failure log and span status.
func (w *Coordinator) RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, c *core.CleanupRequest) {
	w.core.Acct().APICall(s3op.PutObject, c.BackendName) // the PUT that succeeded
	w.DeleteOrEnqueue(ctx, be, c)
}

// NewPendingIntent builds the intent a write is admitted on. ClaimWriteTarget
// fills in the backend, since the same insert checks room. Admission subtracts
// held intents from headroom, so in-progress bytes count for every instance.
//
// size is what lands on the backend and id is the identity a reaper-promoted
// object answers HEAD with. The intent id also names the copy's own storage
// path, so cleaning it up can only remove this intent's bytes.
func NewPendingIntent(key string, size int64, form *core.StoredForm, id *core.ObjectIdentity) *core.PendingObject {
	p := &core.PendingObject{
		IntentID:  audit.NewID(),
		ObjectKey: key,
		SizeBytes: size,
		Identity:  id,
	}
	p.StorageKey = internalkey.StorageKey(key, p.IntentID)
	p.ApplyStoredForm(form)
	return p
}

// NewStorageKey mints a unique path for a write with no pending intent, such as
// a replica or a rebalance or drain move.
func NewStorageKey(objectKey string) string {
	return internalkey.StorageKey(objectKey, audit.NewID())
}

// RecordObjectAndPromoteIntent commits the object location, updates quota, and
// clears the pending intent in one transaction. On failure the intent and the
// backend bytes are left for the pending reaper, which HEADs the backend and
// either promotes or removes the intent. A copy with no intent falls back to
// RecordObjectOrCleanup.
func (w *Coordinator) RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, req *core.RecordObjectRequest) error {
	backendName, err := soleBackend(req)
	if err != nil {
		observe.RecordSpanError(span, err)
		return err
	}
	intentID := req.Copies[0].IntentID
	if intentID == "" {
		// The backend is unavailable here, so we cannot use
		// RecordObjectOrCleanup (which deletes on failure). Resolve via the
		// backend map.
		be, ok := w.core.Backends()[backendName]
		if !ok {
			return fmt.Errorf("backend %s not registered", backendName)
		}
		return w.RecordObjectOrCleanup(ctx, span, be, req)
	}

	displaced, _, err := w.stores.RecordObject(ctx, req)
	if err == nil {
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("committed").Inc()
	}
	if err != nil {
		// The intent stays, so the bytes stay claimed against the backend
		// until whichever pass resolves it - the reaper's promotion or its
		// removal - settles what they are worth.
		w.log.ErrorContext(ctx, "recordObject failed; intent left for reaper",
			"key", req.Key, "backend", backendName, "intent_id", intentID, "error", err)
		// The successful PUT against the backend still consumed an API
		// call. The success-path usage record runs only when this returns
		// nil, so account for it here.
		w.core.Acct().APICall(s3op.PutObject, backendName)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}
	w.cleanupDisplacedCopies(ctx, req.Key, backendName, displaced)
	return nil
}

// CommitCompanionCopy records an extra copy whose upload finished after the
// client was answered, and cleans up after it when a newer write took the key
// first. The copy is added to the key rather than replacing what it holds, so
// the copy that answered the client stays. It reports whether the copy was
// recorded; a copy discarded because a newer write took the key is not an
// error.
func (w *Coordinator) CommitCompanionCopy(ctx context.Context, p *core.PendingObject) (recorded bool, err error) {
	result, displaced, _, err := w.stores.CommitCompanionCopy(ctx, p)
	if err != nil {
		// The intent stays, so the reaper resolves the copy on a later tick -
		// discarding its bytes, since an extra copy is never promoted.
		w.log.ErrorContext(ctx, "commit of a further copy failed; intent left for reaper",
			"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID, logfmt.Err(err))
		w.core.Acct().APICall(s3op.PutObject, p.BackendName)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyFailed).Inc()
		return false, err
	}
	if result == core.CompanionCopyCommitted {
		w.core.Acct().Ingress(s3op.PutObject, p.BackendName, p.SizeBytes)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyCommitted).Inc()
		return true, nil
	}
	w.log.WarnContext(ctx, "a newer write took the key while a further copy was uploading; discarding it",
		"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID)
	w.core.Acct().APICall(s3op.PutObject, p.BackendName)
	w.DeleteDisplaced(ctx, p.ObjectKey, displaced)
	telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyUntrusted).Inc()
	return false, nil
}

// The outcomes ReplicationWriteCopiesTotal counts, shared with the write path
// so the label set is written once.
const (
	WriteCopyCommitted = "committed"
	WriteCopyUntrusted = "untrusted"
	WriteCopyFailed    = "failed"
)

// cleanupDisplacedCopies removes the copies an overwrite displaced and audits
// the overwrite.
func (w *Coordinator) cleanupDisplacedCopies(ctx context.Context, key, newBackend string, displaced []core.DeletedCopy) {
	w.DeleteDisplaced(ctx, key, displaced)

	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", newBackend),
			slog.Int("displaced_copies", len(displaced)),
		)
	}
}

// DeleteDisplaced removes each copy's bytes at the path its own row or intent
// named. It deletes per copy rather than per key, because two writes of one key
// on one backend hold two paths and each cleanup must reach only its own bytes.
func (w *Coordinator) DeleteDisplaced(ctx context.Context, key string, displaced []core.DeletedCopy) {
	reqs := make([]*core.CleanupRequest, len(displaced))
	for i, dc := range displaced {
		// The store labels bytes it cleared for a reason of its own - an intent
		// this write superseded, rather than a copy it replaced - so an operator
		// reading the cleanup queue can tell which is which.
		reqs[i] = dc.Cleanup(key, reasonOverwriteDisplaced)
	}
	w.DeleteAllOrEnqueue(ctx, reqs)
}

// DeleteOrEnqueue deletes the bytes at c's storage key, and on failure enqueues
// the path for background retry and tracks SizeBytes as orphan bytes. Callers
// take the storage key from the row or intent that recorded the copy, so the
// delete cannot reach another write's bytes under the same key. Deletes are not
// gated on usage limits: refusing one over budget would leave an operator
// unable to get back under it.
func (w *Coordinator) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, c *core.CleanupRequest) {
	w.settleDelete(ctx, c, w.core.Delete(ctx, c.BackendName, be, core.StoragePath(c.ObjectKey, c.StorageKey)))
}

// DeleteAllOrEnqueue deletes every request's bytes, each backend's in one
// batched delete where the backend supports it and the backends in parallel,
// then settles each request as DeleteOrEnqueue does. A request naming an
// unknown backend is logged and skipped.
func (w *Coordinator) DeleteAllOrEnqueue(ctx context.Context, reqs []*core.CleanupRequest) {
	byBackend := make(map[string][]*core.CleanupRequest)
	for _, c := range reqs {
		byBackend[c.BackendName] = append(byBackend[c.BackendName], c)
	}
	groups := slices.Collect(maps.Values(byBackend))
	workerpool.Run(ctx, len(groups), groups, func(ctx context.Context, group []*core.CleanupRequest) {
		name := group[0].BackendName
		be, ok := w.core.Backends()[name]
		if !ok {
			w.log.WarnContext(ctx, "delete target backend not found", "backend", name, "copies", len(group))
			return
		}
		paths := make([]string, len(group))
		for i, c := range group {
			paths[i] = core.StoragePath(c.ObjectKey, c.StorageKey)
		}
		failed := w.core.DeleteMany(ctx, name, be, paths)
		for i, c := range group {
			w.settleDelete(ctx, c, failed[paths[i]])
		}
	})
}

// settleDelete finishes one delete: nothing to do when it succeeded or the
// bytes were already absent, otherwise the path is queued for retry. A 404 is
// not queued, since the cleanup worker would only rediscover it as a no-op.
func (w *Coordinator) settleDelete(ctx context.Context, c *core.CleanupRequest, err error) {
	if err == nil {
		return
	}
	if backend.IsNotFound(err) {
		w.log.InfoContext(ctx, "delete target already absent on backend, skipping cleanup enqueue",
			"backend", c.BackendName, "key", c.ObjectKey, "storage_key", c.StorageKey, "reason", c.Reason)
		return
	}
	w.log.WarnContext(ctx, "failed to delete object, enqueuing cleanup",
		"backend", c.BackendName, "key", c.ObjectKey, "storage_key", c.StorageKey, "reason", c.Reason, "error", err)
	w.EnqueueCleanup(ctx, c)
}

// EnqueueCleanup queues a failed cleanup for retry and adds its size to
// orphan_bytes. It is best-effort: a failure increments
// s3o_cleanup_enqueue_failures_total and emits a storage.OrphanEnqueueFailed
// audit event naming the object, which reconcile recovers later.
func (w *Coordinator) EnqueueCleanup(ctx context.Context, c *core.CleanupRequest) {
	if err := w.stores.EnqueueCleanup(ctx, c); err != nil {
		w.recordEnqueueFailure(ctx, c, "enqueue", err)
		return
	}
	if c.SizeBytes > 0 {
		if err := w.stores.IncrementOrphanBytes(ctx, c.BackendName, c.SizeBytes); err != nil {
			w.recordEnqueueFailure(ctx, c, "orphan_bytes", err)
		}
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(c.Reason).Inc()
}

// recordEnqueueFailure increments the failure counter, emits an audit
// event carrying enough attributes to identify the specific orphan,
// and logs an error, for both failure stages (enqueue and orphan_bytes).
func (w *Coordinator) recordEnqueueFailure(ctx context.Context, c *core.CleanupRequest, stage string, err error) {
	telemetry.CleanupEnqueueFailuresTotal.WithLabelValues(c.BackendName, c.Reason, stage).Inc()
	audit.Log(ctx, "storage.OrphanEnqueueFailed",
		slog.String("backend", c.BackendName),
		slog.String("key", c.ObjectKey),
		slog.String("storage_key", c.StorageKey),
		slog.String("reason", c.Reason),
		slog.String("stage", stage),
		slog.Int64("size", c.SizeBytes),
		slog.String("error", err.Error()),
	)
	w.log.ErrorContext(ctx, "orphan cleanup enqueue failed (best-effort)",
		"backend", c.BackendName, "key", c.ObjectKey, "storage_key", c.StorageKey,
		"reason", c.Reason, "stage", stage, "error", err)
}

// -------------------------------------------------------------------------
// SHARED OBJECT MOVE PRIMITIVE
// -------------------------------------------------------------------------

// ErrMoveStale signals that MoveObject was raced: the object was already moved
// or deleted, and the destination bytes have been cleaned up. Callers count it
// as skipped rather than as an error.
var ErrMoveStale = errors.New("object already moved or deleted")

// MoveRequest bundles the inputs to a single src -> dest object move.
// SizeBytes is the caller's estimate, used only by the orphan-cleanup paths;
// the success path charges the size the move committed. SrcStorageKey comes
// from the source row, and DestStorageKey is a fresh path minted for the move,
// so each cleanup path deletes exactly its own bytes.
type MoveRequest struct {
	Key       string
	SizeBytes int64

	SrcBackend  backend.ObjectBackend
	SrcName     string
	DestBackend backend.ObjectBackend
	DestName    string

	SrcStorageKey  string
	DestStorageKey string

	Reasons MoveReasonProfile
}

// MoveReasonProfile groups the cleanup-queue reasons a move emits. Orphan
// labels destination bytes left when MoveObjectLocation errors after the copy,
// and StaleOrphan those left when it reports the row was raced.
type MoveReasonProfile struct {
	Orphan       string
	StaleOrphan  string
	SourceDelete string // the source-side delete after a successful move
}

// RebalanceMoveReasons and DrainMoveReasons are the cleanup-queue reason
// profiles for the two subsystems that move objects.
var (
	RebalanceMoveReasons = MoveReasonProfile{
		Orphan:       "rebalance_orphan",
		StaleOrphan:  "rebalance_stale_orphan",
		SourceDelete: "rebalance_source_delete",
	}
	DrainMoveReasons = MoveReasonProfile{
		Orphan:       "drain_orphan",
		StaleOrphan:  "drain_stale_orphan",
		SourceDelete: "drain_source_delete",
	}
)

// destCleanup describes the bytes this move put on the destination, for the two
// paths that have to take them back off again. The size is the caller's
// estimate, which is all either path has: neither of them completed a metadata
// commit, so no authoritative size was ever settled.
func (r *MoveRequest) destCleanup(reason string) *core.CleanupRequest {
	return &core.CleanupRequest{
		BackendName: r.DestName,
		ObjectKey:   r.Key,
		StorageKey:  r.DestStorageKey,
		Reason:      reason,
		SizeBytes:   r.SizeBytes,
	}
}

// MoveObject moves one object from src to dest for drain and rebalance: it
// copies the bytes, swaps the location row with MoveObjectLocation, and then
// deletes the source copy. If the swap errors or was raced, the destination
// bytes are cleaned up instead. Returns the moved size, ErrMoveStale when the
// swap was raced, or the wrapped failure, including a refused admission.
func (w *Coordinator) MoveObject(ctx context.Context, req *MoveRequest) (int64, error) {
	src := backend.CopyEndpoint{Name: req.SrcName, Backend: req.SrcBackend}
	dst := backend.CopyEndpoint{Name: req.DestName, Backend: req.DestBackend}
	if _, err := w.core.StreamCopy(ctx, src, dst, req.SrcStorageKey, req.DestStorageKey, req.SizeBytes); err != nil {
		return 0, fmt.Errorf("stream copy %s -> %s: %w", req.SrcName, req.DestName, err)
	}

	movedSize, err := w.stores.MoveObjectLocation(ctx, &core.MoveLocation{
		ObjectKey:   req.Key,
		FromBackend: req.SrcName,
		ToBackend:   req.DestName,
		StorageKey:  req.DestStorageKey,
	})
	if err != nil {
		// Destination has the bytes but the metadata CAS failed;
		// enqueue the orphan so the cleanup worker collects it.
		w.DeleteOrEnqueue(ctx, req.DestBackend, req.destCleanup(req.Reasons.Orphan))
		return 0, fmt.Errorf("move object location %s -> %s: %w", req.SrcName, req.DestName, err)
	}
	if movedSize == 0 {
		// Raced: another process moved or deleted the row. The
		// destination bytes are orphaned; enqueue them so the cleanup
		// worker collects them.
		w.DeleteOrEnqueue(ctx, req.DestBackend, req.destCleanup(req.Reasons.StaleOrphan))
		return 0, ErrMoveStale
	}

	// Charged at the size the move committed. Egress and Ingress each record
	// their own API call, and DeleteOrEnqueue records the source DELETE.
	w.DeleteOrEnqueue(ctx, req.SrcBackend, &core.CleanupRequest{
		BackendName: req.SrcName,
		ObjectKey:   req.Key,
		StorageKey:  req.SrcStorageKey,
		Reason:      req.Reasons.SourceDelete,
		SizeBytes:   movedSize,
	})
	w.core.Acct().Egress(s3op.GetObject, req.SrcName, movedSize)
	w.core.Acct().Ingress(s3op.PutObject, req.DestName, movedSize)
	// Both ends moved at the size the CAS committed, charged by the move's own
	// transaction so neither window exists where the bytes are counted on
	// neither backend or on both.
	return movedSize, nil
}

// ClaimUploadTarget picks the backend a multipart upload will live on and
// records the upload there, setting params.BackendName to the backend that
// accepted it. Returns ErrInsufficientStorage when no backend is eligible, or
// the classified selection error.
//
// No bytes are claimed, since each part is counted by its own multipart_parts
// row as it arrives. The insert still decides, so a draining backend declines
// the upload and the next candidate is tried.
func (w *Coordinator) ClaimUploadTarget(ctx context.Context, span trace.Span, operation s3op.Operation, params *core.CreateMultipartUploadParams) (string, error) {
	eligible := w.core.EligibleForWrite([]s3op.Operation{operation}, 0, 0)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.claimFirst(eligible, func(name string) (bool, error) {
		params.BackendName = name
		return w.stores.CreateMultipartUpload(ctx, params)
	})
	if err != nil {
		return "", w.core.ClassifyWriteError(span, operation.String(), err)
	}
	return name, nil
}

// RankReplicaTargets orders the destinations a replication copy may go to,
// emptiest first under the same routing strategy a normal write uses, excluding
// backends that already hold a copy. An empty result means nothing is
// eligible. Room is decided by the conditional insert that records the copy,
// so a caller walks this list until one accepts.
func (w *Coordinator) RankReplicaTargets(size int64, exclusion map[string]bool) []string {
	eligible := w.core.EligibleForWrite([]s3op.Operation{s3op.PutObject}, 0, size)
	filtered := slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		return exclusion[name]
	})
	if len(filtered) == 0 {
		return nil
	}
	return w.rankForWrite(w.core.Quota(), filtered)
}
