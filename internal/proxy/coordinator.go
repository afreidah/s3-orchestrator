// -------------------------------------------------------------------------------
// Write Coordinator - Shared Write-Path Helpers
//
// Author: Alex Freidah
//
// Owns the helpers that combine the per-role store views with the backendCore
// infra primitives to record objects, promote pending intents, enqueue
// cleanups, and pick write targets. ObjectManager and MultipartManager hold a
// *writeCoordinator instead of a *BackendManager back-pointer, so each
// manager is fully initialised at construction time without post-construction
// patching.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

// writeCoordinator bundles the backendCore infra with the metadata-store
// contract and the pending-pattern flag so the write-path helpers can be
// expressed as plain methods on a value owned by BackendManager. The
// managers hold a *writeCoordinator rather than a *BackendManager
// back-pointer, eliminating the post-construction wiring step.
type writeCoordinator struct {
	*backendCore
	stores         core.MetadataStore
	pendingEnabled bool
}

// newWriteCoordinator constructs a writeCoordinator. core must be the
// same *backendCore embedded in BackendManager and the per-domain
// managers so admission, usage, drain, and backend lookup observe a
// single source of truth.
func newWriteCoordinator(core *backendCore, stores core.MetadataStore, pendingEnabled bool) *writeCoordinator {
	return &writeCoordinator{
		backendCore:    core,
		stores:         stores,
		pendingEnabled: pendingEnabled,
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// selectBackendForWrite picks the target backend for a write operation
// using the configured routing strategy. "pack" returns the first backend
// with space, "spread" returns the least-utilized backend.
func (w *writeCoordinator) selectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error) {
	if w.routingStrategy == config.RoutingSpread {
		return w.stores.GetLeastUtilizedBackend(ctx, size, eligible)
	}
	return w.stores.GetBackendWithSpace(ctx, size, eligible)
}

// selectWriteTarget picks a backend for a write operation, combining
// eligibility filtering, backend selection, and error classification
// into a single call. Returns ErrInsufficientStorage when no backend can
// accept the write, or the classified error from the routing query.
func (w *writeCoordinator) selectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error) {
	eligible := w.eligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.selectBackendForWrite(ctx, size, eligible)
	if err != nil {
		return "", w.classifyWriteError(span, operation, err)
	}
	return name, nil
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// recordObjectOrCleanup calls RecordObject and, on failure, deletes the
// orphaned object from the backend. On success, enqueues cleanup for any
// displaced copies on other backends (from overwrites). Updates the
// tracing span on error.
func (w *writeCoordinator) recordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error {
	displaced, err := w.stores.RecordObject(ctx, key, backendName, size, enc)
	if err != nil {
		w.Log().ErrorContext(ctx, "recordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		w.recoverFromRecordFailure(ctx, be, backendName, key, "orphan_record_failed", size)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	w.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// recoverFromRecordFailure runs the post-record-failure cleanup sequence
// shared by recordObjectOrCleanup and the multipart UploadPart record
// path. Accounts for both API calls the failure path made (the original
// PUT and the cleanup DELETE) regardless of whether the cleanup succeeds.
// On cleanup failure the orphan is enqueued for the cleanup-queue worker
// with the supplied reason. Callers are responsible for the failure log
// message and span status before/after this call.
func (w *writeCoordinator) recoverFromRecordFailure(ctx context.Context, be backend.ObjectBackend, backendName, key, cleanupReason string, size int64) {
	w.usage.Record(backendName, 1, 0, 0) // PUT that succeeded
	delErr := w.DeleteWithTimeout(ctx, be, key)
	w.usage.Record(backendName, 1, 0, 0) // cleanup DELETE
	if delErr != nil {
		w.Log().ErrorContext(ctx, "failed to clean up orphaned object",
			"key", key, "backend", backendName, "error", delErr)
		w.enqueueCleanup(ctx, backendName, key, cleanupReason, size)
	}
}

// insertPendingIntent records an in-flight PUT intent before the backend
// upload. Returns the generated intent ID, or empty string if no pending
// store is configured (in which case the legacy delete-on-record-failure
// path remains in effect for that PUT). A failure to insert the intent
// while pending tracking is configured fails the PUT - proceeding without
// the intent would reintroduce the data-loss window the pattern exists to
// close.
func (w *writeCoordinator) insertPendingIntent(ctx context.Context, key, backendName string, size int64, enc *core.EncryptionMeta) (string, error) {
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

// recordObjectAndPromoteIntent commits the object location, updates
// quota, and clears the pending intent in a single transaction. On
// failure, the pending row is left in place and the backend bytes are
// NOT deleted: the pending reaper resolves the intent on a later tick by
// HEADing the backend, promoting the metadata if the bytes are present
// and removing the intent if they are absent.
//
// When intentID is empty (no pending store configured) this falls back
// to the legacy recordObjectOrCleanup behavior so existing call sites
// and tests retain their previous semantics.
func (w *writeCoordinator) recordObjectAndPromoteIntent(ctx context.Context, span trace.Span, key, backendName string, size int64, enc *core.EncryptionMeta, intentID string) error {
	if intentID == "" {
		// No pending tracking - caller already wrote bytes, fall back to the
		// legacy path. The backend is unavailable here, so we cannot use
		// recordObjectOrCleanup (which deletes on failure). Resolve via the
		// backend map.
		be, ok := w.backends[backendName]
		if !ok {
			return fmt.Errorf("backend %s not registered", backendName)
		}
		return w.recordObjectOrCleanup(ctx, span, be, key, backendName, size, enc)
	}

	displaced, err := w.stores.RecordObjectAndClearPending(ctx, key, backendName, size, enc, intentID)
	if err == nil {
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("committed").Inc()
	}
	if err != nil {
		w.Log().ErrorContext(ctx, "recordObjectAndClearPending failed; intent left for reaper",
			"key", key, "backend", backendName, "intent_id", intentID, "error", err)
		// The successful PUT against the backend still consumed an API
		// call. The success-path usage record runs only when this returns
		// nil, so account for it here.
		w.usage.Record(backendName, 1, 0, 0)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	w.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// cleanupDisplacedCopies removes stale copies on other backends displaced
// by an overwrite. Shared between recordObjectOrCleanup and
// recordObjectAndPromoteIntent (the original code duplicated this loop).
func (w *writeCoordinator) cleanupDisplacedCopies(ctx context.Context, key, newBackend string, displaced []core.DeletedCopy) {
	for _, dc := range displaced {
		dcBackend, ok := w.backends[dc.BackendName]
		if !ok {
			w.Log().WarnContext(ctx, "displaced copy backend not found",
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
func (w *writeCoordinator) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	err := w.DeleteWithTimeout(ctx, be, key)
	w.usage.Record(backendName, 1, 0, 0)
	if err != nil {
		w.Log().WarnContext(ctx, "failed to delete object, enqueuing cleanup",
			"backend", backendName, "key", key, "reason", reason, "error", err)
		w.enqueueCleanup(ctx, backendName, key, reason, sizeBytes)
	}
}

// enqueueCleanup adds a failed cleanup operation to the retry queue and
// increments orphan_bytes so the write path accounts for the physically
// unreleased space. Best-effort: if the enqueue or orphan update fails
// (e.g. DB down), logs the error and moves on since the circuit breaker
// is already handling DB outages.
func (w *writeCoordinator) enqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) {
	if err := w.stores.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes); err != nil {
		w.Log().ErrorContext(ctx, "failed to enqueue cleanup (best-effort)",
			"backend", backendName, "key", objectKey, "reason", reason, "error", err)
		return
	}
	if sizeBytes > 0 {
		if err := w.stores.IncrementOrphanBytes(ctx, backendName, sizeBytes); err != nil {
			w.Log().ErrorContext(ctx, "failed to increment orphan bytes (best-effort)",
				"backend", backendName, "key", objectKey, "size", sizeBytes, "error", err)
		}
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}
