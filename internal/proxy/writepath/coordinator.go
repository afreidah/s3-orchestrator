// -------------------------------------------------------------------------------
// Write Coordinator - Shared Write-Path Helpers
//
// Author: Alex Freidah
//
// Owns the helpers that combine the per-role store views with the backendCore
// infra primitives to record objects, promote pending intents, enqueue
// cleanups, and pick write targets. ObjectManager and MultipartManager hold a
// *Coordinator instead of a *BackendManager back-pointer, so each
// manager is fully initialised at construction time without post-construction
// patching.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

// Coordinator bundles the infrastructure subset (WritepathCore) with
// the metadata-store contract and the pending-pattern flag so the
// write-path helpers can be expressed as plain methods on a value owned
// by BackendManager. The managers hold a *Coordinator rather than a
// *BackendManager back-pointer, eliminating the post-construction
// wiring step.
type Coordinator struct {
	core           WritepathCore // infrastructure subset: backends, usage, routing, eligibility, error classification, delete-with-timeout
	stores         core.MetadataStore
	pendingEnabled bool
	log            *slog.Logger
}

// New constructs a Coordinator. The supplied core must observe the same
// admission, usage, drain, and backend state as the *infra.Core embedded
// in BackendManager (in production they are the same instance). The
// component-scoped logger is built in the constructor body per the
// project's logging convention.
func New(core WritepathCore, stores core.MetadataStore, pendingEnabled bool) *Coordinator {
	return &Coordinator{
		core:           core,
		stores:         stores,
		pendingEnabled: pendingEnabled,
		log:            slog.Default().With(logfmt.Component("writepath")),
	}
}

// SetPendingEnabledForTest toggles the pending-pattern flag. Test-only:
// production wiring always passes the flag through New.
func (w *Coordinator) SetPendingEnabledForTest(enabled bool) {
	w.pendingEnabled = enabled
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// SelectBackendForWrite picks the target backend for a write operation
// using the configured routing strategy. "pack" returns the first backend
// with space, "spread" returns the least-utilized backend.
func (w *Coordinator) SelectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error) {
	if w.core.RoutingStrategy() == config.RoutingSpread {
		return w.stores.GetLeastUtilizedBackend(ctx, size, eligible)
	}
	return w.stores.GetBackendWithSpace(ctx, size, eligible)
}

// SelectWriteTarget picks a backend for a write operation, combining
// eligibility filtering, backend selection, and error classification
// into a single call. Returns ErrInsufficientStorage when no backend can
// accept the write, or the classified error from the routing query.
func (w *Coordinator) SelectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error) {
	eligible := w.core.EligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.SelectBackendForWrite(ctx, size, eligible)
	if err != nil {
		return "", w.core.ClassifyWriteError(span, operation, err)
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
func (w *Coordinator) RecordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error {
	displaced, err := w.stores.RecordObject(ctx, key, backendName, size, enc)
	if err != nil {
		w.log.ErrorContext(ctx, "recordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		w.RecoverFromRecordFailure(ctx, be, backendName, key, "orphan_record_failed", size)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	w.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// RecoverFromRecordFailure runs the post-record-failure cleanup sequence
// shared by RecordObjectOrCleanup and the multipart UploadPart record
// path. Accounts for both API calls the failure path made (the original
// PUT and the cleanup DELETE) regardless of whether the cleanup succeeds.
// On cleanup failure the orphan is enqueued for the cleanup-queue worker
// with the supplied reason. Callers are responsible for the failure log
// message and span status before/after this call.
func (w *Coordinator) RecoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64) {
	w.core.Acct().APICall(backendName) // PUT that succeeded
	delErr := w.core.DeleteWithTimeout(ctx, be, key)
	w.core.Acct().APICall(backendName) // cleanup DELETE
	if delErr != nil {
		w.log.ErrorContext(ctx, "failed to clean up orphaned object",
			"key", key, "backend", backendName, "error", delErr)
		w.EnqueueCleanup(ctx, backendName, key, cleanupReason, size)
	}
}

// InsertPendingIntent records an in-flight PUT intent before the backend
// upload. Returns the generated intent ID, or empty string if no pending
// store is configured (in which case the legacy delete-on-record-failure
// path remains in effect for that PUT). A failure to insert the intent
// while pending tracking is configured fails the PUT - proceeding without
// the intent would reintroduce the data-loss window the pattern exists to
// close.
func (w *Coordinator) InsertPendingIntent(ctx context.Context, key, backendName string, size int64, enc *core.EncryptionMeta) (string, error) {
	if !w.pendingEnabled {
		return "", nil
	}
	intentID := audit.NewID()
	p := core.PendingObject{
		IntentID:    intentID,
		ObjectKey:   key,
		BackendName: backendName,
		SizeBytes:   size,
	}
	if enc != nil {
		p.Encrypted = enc.Encrypted
		p.EncryptionKey = enc.EncryptionKey
		p.KeyID = enc.KeyID
		p.PlaintextSize = enc.PlaintextSize
		p.ContentHash = enc.ContentHash
	}
	if err := w.stores.InsertPending(ctx, &p); err != nil {
		return "", fmt.Errorf("insert pending intent: %w", err)
	}
	telemetry.PendingIntentsEnqueuedTotal.Inc()
	return intentID, nil
}

// RecordObjectAndPromoteIntent commits the object location, updates
// quota, and clears the pending intent in a single transaction. On
// failure, the pending row is left in place and the backend bytes are
// NOT deleted: the pending reaper resolves the intent on a later tick by
// HEADing the backend, promoting the metadata if the bytes are present
// and removing the intent if they are absent.
//
// When intentID is empty (no pending store configured) this falls back
// to the legacy RecordObjectOrCleanup behavior so existing call sites
// and tests retain their previous semantics.
func (w *Coordinator) RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, key, backendName string, size int64, enc *core.EncryptionMeta, intentID string) error {
	if intentID == "" {
		// No pending tracking - caller already wrote bytes, fall back to the
		// legacy path. The backend is unavailable here, so we cannot use
		// RecordObjectOrCleanup (which deletes on failure). Resolve via the
		// backend map.
		be, ok := w.core.Backends()[backendName]
		if !ok {
			return fmt.Errorf("backend %s not registered", backendName)
		}
		return w.RecordObjectOrCleanup(ctx, span, be, key, backendName, size, enc)
	}

	displaced, err := w.stores.RecordObjectAndClearPending(ctx, key, backendName, size, enc, intentID)
	if err == nil {
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("committed").Inc()
	}
	if err != nil {
		w.log.ErrorContext(ctx, "recordObjectAndClearPending failed; intent left for reaper",
			"key", key, "backend", backendName, "intent_id", intentID, "error", err)
		// The successful PUT against the backend still consumed an API
		// call. The success-path usage record runs only when this returns
		// nil, so account for it here.
		w.core.Acct().APICall(backendName)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	w.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// cleanupDisplacedCopies removes stale copies on other backends displaced
// by an overwrite. Shared between RecordObjectOrCleanup and
// RecordObjectAndPromoteIntent (the original code duplicated this loop).
func (w *Coordinator) cleanupDisplacedCopies(ctx context.Context, key, newBackend string, displaced []core.DeletedCopy) {
	for _, dc := range displaced {
		dcBackend, ok := w.core.Backends()[dc.BackendName]
		if !ok {
			w.log.WarnContext(ctx, "displaced copy backend not found",
				"backend", dc.BackendName, "key", key)
			continue
		}
		w.DeleteOrEnqueue(ctx, dcBackend, dc.BackendName, key, "overwrite_displaced", dc.SizeBytes)
	}

	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", newBackend),
			slog.Int("displaced_copies", len(displaced)),
		)
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
	err := w.core.DeleteWithTimeout(ctx, be, key)
	w.core.Acct().APICall(backendName)
	if err != nil {
		w.log.WarnContext(ctx, "failed to delete object, enqueuing cleanup",
			"backend", backendName, "key", key, "reason", reason, "error", err)
		w.EnqueueCleanup(ctx, backendName, key, reason, sizeBytes)
	}
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
// connectivity is restored. See docs/admin-guide.md for the runbook.
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
