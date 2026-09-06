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
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
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

// SelectBackendForWrite picks the target backend for a write operation using
// the configured routing strategy and claims the bytes on it. "pack" takes the
// first eligible backend with room, "spread" the least utilized one. Returns
// ErrNoSpaceAvailable when no candidate has room.
//
// Both the fit test and the ranking read the in-memory tracker rather than
// The ranking reads the in-memory snapshot, which is allowed to be stale: a
// slightly wrong order costs an uneven spread that the next refresh corrects.
// The fit test is not allowed to be stale, so it is the intent insert itself -
// one statement that claims the bytes only if the backend's live rows still
// have room. Ranking proposes; the insert decides.
//
// A candidate the insert declines is skipped rather than fatal: another backend
// may still have room. Returns ErrNoSpaceAvailable when none does.
func (w *Coordinator) ClaimWriteTarget(ctx context.Context, p *core.PendingObject, eligible []string) (string, error) {
	for _, name := range w.rankForWrite(w.core.Quota(), eligible) {
		p.BackendName = name
		fits, err := w.stores.InsertPendingIfFits(ctx, p)
		if err != nil {
			return "", fmt.Errorf("claim write target: %w", err)
		}
		if fits {
			// Tell the ranking what this instance just placed, or every write
			// in the interval before the next reload ranks the candidates
			// identically and spread stops spreading.
			w.core.Quota().NotePlacement(name, p.SizeBytes)
			telemetry.PendingIntentsEnqueuedTotal.Inc()
			return name, nil
		}
		telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(name).Inc()
	}
	return "", core.ErrNoSpaceAvailable
}

// ClaimWriteCopies claims a backend for each of the supplied intents, which are
// the copies one write places at the same time. Each intent is claimed by the
// same conditional insert a single-copy write uses, so every copy's bytes are
// held against its own backend for as long as its upload runs.
//
// Returns the intents that got a backend, in claim order, with BackendName set.
// A candidate that declines is skipped and a claim short of what was asked for
// is not an error: fewer copies is a write the replicator finishes later, and
// the caller decides that placing none at all is what fails the write. For the
// same reason a database error once a copy has been claimed ends the loop
// rather than the write.
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

// SelectWriteTarget picks a backend for a write and claims it, combining
// eligibility filtering, ranking, admission, and error classification into a
// single call. Returns ErrInsufficientStorage when no backend can accept the
// write, or the classified selection error.
//
// The claim is the pending intent, which the caller owns from here: it is
// cleared by the transaction that records the object, and left for the reaper
// on any path that gives up.
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
// orphaned object from the backend. On success, enqueues cleanup for any
// displaced copies on other backends (from overwrites). Updates the
// tracing span on error.
//
// Nothing is settled against a counter here. The transaction charged the bytes
// to the backend's stripes and cleared the intent that had been holding them,
// so the ledger is already correct the moment it commits.
//
// The supplied backend handle is what the failure path deletes from, so this
// records one copy. A write placing several needs recovery that knows which of
// them landed, which is the caller's to hold rather than this helper's.
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
		w.RecoverFromRecordFailure(ctx, be, backendName, req.Key, "orphan_record_failed", req.Size)
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

// RecoverFromRecordFailure runs the post-record-failure cleanup
// sequence shared by RecordObjectOrCleanup and the multipart
// UploadPart record path. Accounts for both API calls the failure
// path made (the original PUT and the cleanup DELETE) regardless of
// whether the cleanup succeeds; enqueues the orphan with the supplied
// reason on cleanup failure. A backend 404 is treated as idempotent
// success and skips the enqueue so the cleanup queue does not collect
// phantom rows for objects already gone. Callers own the failure log
// message and span status before/after this call.
func (w *Coordinator) RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64) {
	w.core.Acct().APICall(s3op.PutObject, backendName) // PUT that succeeded
	delErr := w.core.DeleteWithTimeout(ctx, be, key)
	w.core.Acct().APICall(s3op.DeleteObject, backendName) // cleanup DELETE
	if delErr == nil {
		return
	}
	// A 404 means the backend already agrees the object is gone, which
	// is the desired end state. Skip enqueueing so we don't seed the
	// cleanup queue with rows the cleanup worker would also have to
	// recognise as no-ops.
	if backend.IsNotFound(delErr) {
		w.log.InfoContext(ctx, "orphan cleanup target already absent on backend",
			"key", key, "backend", backendName, "reason", cleanupReason)
		return
	}
	w.log.ErrorContext(ctx, "failed to clean up orphaned object",
		"key", key, "backend", backendName, "error", delErr)
	w.EnqueueCleanup(ctx, backendName, key, cleanupReason, size)
}

// NewPendingIntent builds the intent a write will be admitted on. The backend
// is left unset: which one it names is decided by ClaimWriteTarget, because the
// insert that writes this row is the same statement that tests whether the
// backend has room for it.
//
// The row is not only a recovery breadcrumb. Admission subtracts the intents a
// backend is holding from its headroom, so the bytes of a write in progress
// occupy the backend for every instance rather than only the one performing it.
// That is why there is no longer a mode without it.
//
// size is what will land on the backend, which is what quota is reconciled
// against if this intent is recovered rather than committed. id is what the
// client will be told the object is; carrying it here is what lets a
// reaper-promoted object answer a HEAD without re-learning it.
func NewPendingIntent(key string, size int64, form *core.StoredForm, id *core.ObjectIdentity) *core.PendingObject {
	p := &core.PendingObject{
		IntentID:  audit.NewID(),
		ObjectKey: key,
		SizeBytes: size,
		Identity:  id,
	}
	p.ApplyStoredForm(form)
	return p
}

// RecordObjectAndPromoteIntent commits the object location, updates
// quota, and clears the pending intent in a single transaction. On
// failure, the pending row is left in place and the backend bytes are
// NOT deleted: the pending reaper resolves the intent on a later tick by
// HEADing the backend, promoting the metadata if the bytes are present
// and removing the intent if they are absent.
//
// When the copy carries no intent - a caller that wrote bytes without claiming
// one first, which only the assembly paths do - this falls back to
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
// the copy that answered the client stays.
//
// The bytes of an untrusted copy go through the same orphan cleanup as any
// other, because deleting them is the point and where the row came from is not
// something the cleanup queue needs to know.
func (w *Coordinator) CommitCompanionCopy(ctx context.Context, p *core.PendingObject) error {
	result, displaced, _, err := w.stores.CommitCompanionCopy(ctx, p)
	if err != nil {
		// The intent stays, so the reaper resolves the copy on a later tick -
		// discarding its bytes, since an extra copy is never promoted.
		w.log.ErrorContext(ctx, "commit of a further copy failed; intent left for reaper",
			"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID, logfmt.Err(err))
		w.core.Acct().APICall(s3op.PutObject, p.BackendName)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyFailed).Inc()
		return err
	}
	if result == core.CompanionCopyCommitted {
		w.core.Acct().Ingress(s3op.PutObject, p.BackendName, p.SizeBytes)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyCommitted).Inc()
		return nil
	}
	w.log.WarnContext(ctx, "a newer write took the key while a further copy was uploading; discarding it",
		"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID)
	w.core.Acct().APICall(s3op.PutObject, p.BackendName)
	w.deleteDisplaced(ctx, p.ObjectKey, displaced)
	telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyUntrusted).Inc()
	return nil
}

// The outcomes ReplicationWriteCopiesTotal counts, shared with the write path
// so the label set is written once.
const (
	WriteCopyCommitted = "committed"
	WriteCopyUntrusted = "untrusted"
	WriteCopyFailed    = "failed"
)

// cleanupDisplacedCopies removes stale copies on other backends displaced
// by an overwrite. Shared between RecordObjectOrCleanup and
// RecordObjectAndPromoteIntent (the original code duplicated this loop).
func (w *Coordinator) cleanupDisplacedCopies(ctx context.Context, key, newBackend string, displaced []core.DeletedCopy) {
	w.deleteDisplaced(ctx, key, displaced)

	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", newBackend),
			slog.Int("displaced_copies", len(displaced)),
		)
	}
}

// deleteDisplaced removes each copy's bytes from the backend that holds them.
// Shared with the companion-copy path, which discards a copy without any
// overwrite having displaced it and so has nothing to audit.
func (w *Coordinator) deleteDisplaced(ctx context.Context, key string, displaced []core.DeletedCopy) {
	for _, dc := range displaced {
		dcBackend, ok := w.core.Backends()[dc.BackendName]
		if !ok {
			w.log.WarnContext(ctx, "displaced copy backend not found",
				"backend", dc.BackendName, "key", key)
			continue
		}
		// The store labels bytes it cleared for a reason of its own - an intent
		// this write superseded, rather than a copy it replaced - so an operator
		// reading the cleanup queue can tell which is which.
		reason := dc.Reason
		if reason == "" {
			reason = reasonOverwriteDisplaced
		}
		w.DeleteOrEnqueue(ctx, dcBackend, dc.BackendName, key, reason, dc.SizeBytes)
	}
}

// DeleteOrEnqueue attempts to delete an object from a backend. On
// failure it logs a warning and enqueues the key for background retry.
// The standard "best-effort orphan cleanup" primitive used throughout the
// manager: rebalancer, replicator, multipart cleanup, and delete paths.
// sizeBytes is tracked as orphan bytes when the delete is enqueued.
// Always accounts for the cleanup DELETE as one API call against the
// backend's usage counter, regardless of success or failure (the HTTP
// call to the backend was made either way).
func (w *Coordinator) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	// Deliberately not gated on usage limits. A delete is the one operation
	// that reduces what a backend holds, so refusing it over budget would
	// leave an operator unable to get back under one, and a client DELETE
	// that returns without removing the object is simply wrong. The API call
	// is still charged below.
	err := w.core.DeleteWithTimeout(ctx, be, key)
	w.core.Acct().APICall(s3op.DeleteObject, backendName)
	if err == nil {
		return
	}
	// A 404 means the backend already agrees the object is gone, which
	// is the desired end state. Skip enqueueing so we don't seed the
	// cleanup queue with rows the cleanup worker would also have to
	// recognise as no-ops.
	if backend.IsNotFound(err) {
		w.log.InfoContext(ctx, "delete target already absent on backend, skipping cleanup enqueue",
			"backend", backendName, "key", key, "reason", reason)
		return
	}
	w.log.WarnContext(ctx, "failed to delete object, enqueuing cleanup",
		"backend", backendName, "key", key, "reason", reason, "error", err)
	w.EnqueueCleanup(ctx, backendName, key, reason, sizeBytes)
}

// EnqueueCleanup adds a failed cleanup operation to the retry queue and
// increments orphan_bytes so the write path accounts for the physically
// unreleased space. Best-effort: if the enqueue or orphan update fails
// (e.g. DB down), logs the error and moves on since the circuit breaker
// is already handling DB outages.
//
// Failures here mean a backend object exists with no entry in the
// cleanup queue (stage="enqueue") or no matching orphan_bytes increment
// (stage="orphan_bytes"). Both failure modes increment
// s3o_cleanup_enqueue_failures_total and emit a
// storage.OrphanEnqueueFailed audit event so operators can pivot from
// "metric incremented" to the exact backend/key/size, then run
// POST /admin/api/reconcile to recover untracked orphans once DB
// connectivity is restored. See docs/cleanup-and-lifecycle.md for the runbook.
func (w *Coordinator) EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) {
	if err := w.stores.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes); err != nil {
		w.recordEnqueueFailure(ctx, backendName, objectKey, reason, sizeBytes, "enqueue", err)
		return
	}
	if sizeBytes > 0 {
		if err := w.stores.IncrementOrphanBytes(ctx, backendName, sizeBytes); err != nil {
			w.recordEnqueueFailure(ctx, backendName, objectKey, reason, sizeBytes, "orphan_bytes", err)
		}
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}

// recordEnqueueFailure increments the failure counter, emits an audit
// event carrying enough attributes to identify the specific orphan,
// and logs an error. Hoisted so the two failure stages (enqueue vs
// orphan_bytes) share one observability path  -  a future spool
// integration plugs in here.
func (w *Coordinator) recordEnqueueFailure(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64, stage string, err error) {
	telemetry.CleanupEnqueueFailuresTotal.WithLabelValues(backendName, reason, stage).Inc()
	audit.Log(ctx, "storage.OrphanEnqueueFailed",
		slog.String("backend", backendName),
		slog.String("key", objectKey),
		slog.String("reason", reason),
		slog.String("stage", stage),
		slog.Int64("size", sizeBytes),
		slog.String("error", err.Error()),
	)
	w.log.ErrorContext(ctx, "orphan cleanup enqueue failed (best-effort)",
		"backend", backendName, "key", objectKey, "reason", reason, "stage", stage, "error", err)
}

// -------------------------------------------------------------------------
// SHARED OBJECT MOVE PRIMITIVE
// -------------------------------------------------------------------------

// ErrMoveStale signals MoveObject was raced: MoveObjectLocation
// returned movedSize=0, meaning another process (or the same caller
// from a prior tick) already moved or deleted the object. The
// destination has had its now-orphaned bytes enqueued for cleanup via
// req.StaleOrphanReason. Callers treat this as a no-op rather than a
// failure - increment a "stale" / "skipped" counter rather than an
// error counter.
var ErrMoveStale = errors.New("object already moved or deleted")

// MoveRequest bundles the inputs to a single src -> dest object move.
// Callers supply distinct cleanup-queue reason strings per failure
// mode so a future operator triaging the cleanup_queue can tell which
// subsystem orphaned each row.
// SizeBytes is what the caller knew before the move ran, and is used only by
// the orphan-cleanup paths, where MoveObjectLocation never returned the row's
// real size. The success path charges the authoritative movedSize instead.
type MoveRequest struct {
	Key       string
	SizeBytes int64

	SrcBackend  backend.ObjectBackend
	SrcName     string
	DestBackend backend.ObjectBackend
	DestName    string

	Reasons MoveReasonProfile
}

// MoveReasonProfile groups the cleanup-queue reason labels a move emits across
// its three cleanup paths, so callers select a named profile instead of
// repeating the same strings - and the labels stay consistent and typo-free.
//
// Both orphan cases leave bytes on the destination: the first when
// MoveObjectLocation errors after the PUT landed, the second when it reports a
// raced row because another process won.
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

// MoveObject performs a single src -> dest object move with cleanup
// semantics that drain and rebalance share: StreamCopy the source body
// to dest, atomic MoveObjectLocation CAS, orphan cleanup on dest if
// the CAS errors, stale-orphan cleanup on dest if the CAS reports a
// raced row, and on success a source-side DeleteOrEnqueue plus the canonical
// accounting (Egress on src + Ingress on dest).
//
// StreamCopy admits the transfer against both backends' usage limits before
// any bytes move; the bytes themselves are accounted for here, at the size the
// move committed. DeleteOrEnqueue owns the per-backend DELETE API-call tick,
// so this method does NOT call Acct().APICall(...) on the destination cleanup
// or the source delete.
//
// Returns the moved size on success, ErrMoveStale when MoveObjectLocation
// raced, and the wrapped underlying failure otherwise - including a transfer
// either backend had no usage headroom for.
func (w *Coordinator) MoveObject(ctx context.Context, req *MoveRequest) (int64, error) {
	src := backend.CopyEndpoint{Name: req.SrcName, Backend: req.SrcBackend}
	dst := backend.CopyEndpoint{Name: req.DestName, Backend: req.DestBackend}
	if _, err := w.core.StreamCopy(ctx, src, dst, req.Key, req.SizeBytes); err != nil {
		return 0, fmt.Errorf("stream copy %s -> %s: %w", req.SrcName, req.DestName, err)
	}

	movedSize, err := w.stores.MoveObjectLocation(ctx, req.Key, req.SrcName, req.DestName)
	if err != nil {
		// Destination has the bytes but the metadata CAS failed;
		// enqueue the orphan so the cleanup worker collects it.
		w.DeleteOrEnqueue(ctx, req.DestBackend, req.DestName, req.Key, req.Reasons.Orphan, req.SizeBytes)
		return 0, fmt.Errorf("move object location %s -> %s: %w", req.SrcName, req.DestName, err)
	}
	if movedSize == 0 {
		// Raced: another process moved or deleted the row. The
		// destination bytes are orphaned; enqueue them so the cleanup
		// worker collects them.
		w.DeleteOrEnqueue(ctx, req.DestBackend, req.DestName, req.Key, req.Reasons.StaleOrphan, req.SizeBytes)
		return 0, ErrMoveStale
	}

	// Success path: source delete + canonical accounting, charged at the size
	// the move committed rather than the size that crossed the wire. Egress
	// and Ingress include their own single API-call tick (one for the source
	// GET, one for the dest PUT); DeleteOrEnqueue includes the source DELETE
	// tick. No additional Acct().APICall calls.
	w.DeleteOrEnqueue(ctx, req.SrcBackend, req.SrcName, req.Key, req.Reasons.SourceDelete, movedSize)
	w.core.Acct().Egress(s3op.GetObject, req.SrcName, movedSize)
	w.core.Acct().Ingress(s3op.PutObject, req.DestName, movedSize)
	// Both ends moved at the size the CAS committed, charged by the move's own
	// transaction so neither window exists where the bytes are counted on
	// neither backend or on both.
	return movedSize, nil
}

// PickWriteTarget names the backend a write should target without claiming
// anything on it.
//
// For the writes whose bytes are accounted for by rows of their own: a
// multipart create decides where the upload will live long before any part
// exists, and each part is counted against the backend by its own
// multipart_parts row as it arrives. Claiming at create time would hold bytes
// nobody has sent yet.
func (w *Coordinator) PickWriteTarget(span trace.Span, operation s3op.Operation, size int64) (string, error) {
	eligible := w.core.EligibleForWrite([]s3op.Operation{operation}, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	ranked := w.rankForWrite(w.core.Quota(), eligible)
	return ranked[0], nil
}

// RankReplicaTargets orders the destinations a replication copy may go to,
// emptiest first under the same routing strategy a normal write uses, excluding
// backends that already hold a copy. An empty result means nothing is eligible,
// which the caller treats as a skip rather than a failure.
//
// Only the order is decided here. Whether a candidate has room is settled by
// the conditional insert that records the copy, so a caller walks this list
// until one of them accepts the row.
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
