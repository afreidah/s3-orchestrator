// -------------------------------------------------------------------------------
// BackendManager Write-Path Helpers
//
// Author: Alex Freidah
//
// Helpers that combine the per-role store views with the backendCore infra
// primitives to record objects, promote pending intents, enqueue cleanups,
// and pick write targets. They live on *BackendManager rather than
// *backendCore so backendCore can stay a pure infra struct (no store
// dependencies).
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// STORE-ROLE ACCESSORS
// -------------------------------------------------------------------------

// Multipart returns the multipart-upload store role. Exposed for transport
// handlers that need direct upload bookkeeping (e.g. s3api count).
func (m *BackendManager) Multipart() core.MultipartStore { return m.stores.Multipart }

// BackendLifecycle returns the backend-level admin store role. Exposed
// for admin handlers that report per-backend stats or drop backend data.
func (m *BackendManager) BackendLifecycle() core.BackendLifecycleStore {
	return m.stores.BackendLifecycle
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// SelectReplicaTarget picks a target backend for a replication copy using the
// same routing strategy as normal writes. Excludes backends that already hold
// a copy of the object.
func (m *BackendManager) SelectReplicaTarget(ctx context.Context, size int64, exclusion map[string]bool) (string, error) {
	eligible := m.eligibleForWrite(1, 0, size)
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if !exclusion[name] {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return "", nil
	}
	name, err := m.selectBackendForWrite(ctx, size, filtered)
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		return "", nil
	}
	return name, err
}

// selectBackendForWrite picks the target backend for a write operation using
// the configured routing strategy. "pack" returns the first backend with space,
// "spread" returns the least-utilized backend.
func (m *BackendManager) selectBackendForWrite(ctx context.Context, size int64, eligible []string) (string, error) {
	if m.routingStrategy == config.RoutingSpread {
		return m.stores.Quota.GetLeastUtilizedBackend(ctx, size, eligible)
	}
	return m.stores.Quota.GetBackendWithSpace(ctx, size, eligible)
}

// selectWriteTarget picks a backend for a write operation, combining
// eligibility filtering, backend selection, and error classification into
// a single call. Returns ErrInsufficientStorage when no backend can accept
// the write, or the classified error from the routing query.
func (m *BackendManager) selectWriteTarget(ctx context.Context, span trace.Span, operation string, size int64) (string, error) {
	eligible := m.eligibleForWrite(1, 0, size)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation, "write").Inc()
		span.SetStatus(codes.Error, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := m.selectBackendForWrite(ctx, size, eligible)
	if err != nil {
		return "", m.classifyWriteError(span, operation, err)
	}
	return name, nil
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// recordObjectOrCleanup calls RecordObject and, on failure, deletes the orphaned
// object from the backend. On success, enqueues cleanup for any displaced copies
// on other backends (from overwrites). Updates the tracing span on error.
func (m *BackendManager) recordObjectOrCleanup(ctx context.Context, span trace.Span, be backend.ObjectBackend, key, backendName string, size int64, enc *core.EncryptionMeta) error {
	displaced, err := m.stores.Object.RecordObject(ctx, key, backendName, size, enc)
	if err != nil {
		slog.ErrorContext(ctx, "recordObject failed, cleaning up orphan",
			"key", key, "backend", backendName, "error", err)
		// Account for both API calls the failure path made: the PUT that
		// succeeded against the backend (the caller's success-path Record
		// runs only after we return nil) and the cleanup DELETE about to run.
		m.usage.Record(backendName, 1, 0, 0) // PUT
		delErr := m.deleteWithTimeout(ctx, be, key)
		m.usage.Record(backendName, 1, 0, 0) // cleanup DELETE
		if delErr != nil {
			slog.ErrorContext(ctx, "failed to clean up orphaned object",
				"key", key, "backend", backendName, "error", delErr)
			m.enqueueCleanup(ctx, backendName, key, "orphan_record_failed", size)
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	m.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// insertPendingIntent records an in-flight PUT intent before the backend
// upload. Returns the generated intent ID, or empty string if no pending
// store is configured (in which case the legacy delete-on-record-failure
// path remains in effect for that PUT). A failure to insert the intent
// while pending tracking is configured fails the PUT — proceeding without
// the intent would reintroduce the data-loss window the pattern exists to
// close.
func (m *BackendManager) insertPendingIntent(ctx context.Context, key, backendName string, size int64, enc *core.EncryptionMeta) (string, error) {
	if m.stores.Pending == nil {
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
	if err := m.stores.Pending.InsertPending(ctx, &p); err != nil {
		return "", fmt.Errorf("insert pending intent: %w", err)
	}
	telemetry.PendingIntentsEnqueuedTotal.Inc()
	return intentID, nil
}

// recordObjectAndPromoteIntent commits the object location, updates quota,
// and clears the pending intent in a single transaction. On failure, the
// pending row is left in place and the backend bytes are NOT deleted: the
// pending reaper resolves the intent on a later tick by HEADing the
// backend, promoting the metadata if the bytes are present and removing
// the intent if they are absent.
//
// When intentID is empty (no pending store configured) this falls back to
// the legacy recordObjectOrCleanup behavior so existing call sites and
// tests retain their previous semantics.
func (m *BackendManager) recordObjectAndPromoteIntent(ctx context.Context, span trace.Span, key, backendName string, size int64, enc *core.EncryptionMeta, intentID string) error {
	if intentID == "" {
		// No pending tracking — caller already wrote bytes, fall back to the
		// legacy path. The backend is unavailable here, so we cannot use
		// recordObjectOrCleanup (which deletes on failure). Resolve via the
		// backend map.
		be, ok := m.backends[backendName]
		if !ok {
			return fmt.Errorf("backend %s not registered", backendName)
		}
		return m.recordObjectOrCleanup(ctx, span, be, key, backendName, size, enc)
	}

	displaced, err := m.stores.Object.RecordObjectAndClearPending(ctx, key, backendName, size, enc, intentID)
	if err == nil {
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("committed").Inc()
	}
	if err != nil {
		slog.ErrorContext(ctx, "recordObjectAndClearPending failed; intent left for reaper",
			"key", key, "backend", backendName, "intent_id", intentID, "error", err)
		// The successful PUT against the backend still consumed an API
		// call. The success-path usage record runs only when this returns
		// nil, so account for it here.
		m.usage.Record(backendName, 1, 0, 0)
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return fmt.Errorf("failed to record object: %w", err)
	}

	m.cleanupDisplacedCopies(ctx, key, backendName, displaced)
	return nil
}

// cleanupDisplacedCopies removes stale copies on other backends displaced
// by an overwrite. Shared between recordObjectOrCleanup and
// recordObjectAndPromoteIntent (the original code duplicated this loop).
func (m *BackendManager) cleanupDisplacedCopies(ctx context.Context, key, newBackend string, displaced []core.DeletedCopy) {
	for _, dc := range displaced {
		dcBackend, ok := m.backends[dc.BackendName]
		if !ok {
			slog.WarnContext(ctx, "displaced copy backend not found",
				"backend", dc.BackendName, "key", key)
			continue
		}
		m.deleteOrEnqueue(ctx, dcBackend, dc.BackendName, key, "overwrite_displaced", dc.SizeBytes)
	}

	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", newBackend),
			slog.Int("displaced_copies", len(displaced)),
		)
	}
}

// deleteOrEnqueue attempts to delete an object from a backend. On failure
// it logs a warning and enqueues the key for background retry. The
// standard "best-effort orphan cleanup" primitive used throughout the
// manager: rebalancer, replicator, multipart cleanup, and delete paths.
// sizeBytes is tracked as orphan bytes when the delete is enqueued.
func (m *BackendManager) deleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	if err := m.deleteWithTimeout(ctx, be, key); err != nil {
		slog.WarnContext(ctx, "failed to delete object, enqueuing cleanup",
			"backend", backendName, "key", key, "reason", reason, "error", err)
		m.enqueueCleanup(ctx, backendName, key, reason, sizeBytes)
	}
}

// DeleteOrEnqueue is the exported wrapper around deleteOrEnqueue for the
// worker.Ops and drain.Core seams.
func (m *BackendManager) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	m.deleteOrEnqueue(ctx, be, backendName, key, reason, sizeBytes)
}

// enqueueCleanup adds a failed cleanup operation to the retry queue and
// increments orphan_bytes so the write path accounts for the physically
// unreleased space. Best-effort: if the enqueue or orphan update fails
// (e.g. DB down), logs the error and moves on since the circuit breaker
// is already handling DB outages.
func (m *BackendManager) enqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) {
	if err := m.stores.Cleanup.EnqueueCleanup(ctx, backendName, objectKey, reason, sizeBytes); err != nil {
		slog.ErrorContext(ctx, "failed to enqueue cleanup (best-effort)",
			"backend", backendName, "key", objectKey, "reason", reason, "error", err)
		return
	}
	if sizeBytes > 0 {
		if err := m.stores.Cleanup.IncrementOrphanBytes(ctx, backendName, sizeBytes); err != nil {
			slog.ErrorContext(ctx, "failed to increment orphan bytes (best-effort)",
				"backend", backendName, "key", objectKey, "size", sizeBytes, "error", err)
		}
	}
	telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
}
