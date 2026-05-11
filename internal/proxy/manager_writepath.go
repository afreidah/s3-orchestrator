// -------------------------------------------------------------------------------
// BackendManager Write-Path Surface
//
// Author: Alex Freidah
//
// The write-path helpers (selectBackendForWrite, recordObjectOrCleanup,
// insertPendingIntent, recordObjectAndPromoteIntent, recoverFromRecordFailure,
// cleanupDisplacedCopies, enqueueCleanup, DeleteOrEnqueue) live on the
// shared *writeCoordinator in coordinator.go so ObjectManager and
// MultipartManager can use them without a *BackendManager back-pointer.
// This file keeps the thin surface BackendManager exposes externally:
// the metadata-store accessor, the replica-target router, and a
// pass-through to DeleteOrEnqueue for worker/drain interfaces that
// already accept *BackendManager.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"errors"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// STORE-ROLE ACCESSORS
// -------------------------------------------------------------------------

// Stores returns the wrapped metadata-store contract. Exposed for
// transport handlers that need direct store access (multipart bookkeeping
// in s3api, per-backend stats / data drop in admin).
func (m *BackendManager) Stores() core.MetadataStore { return m.stores }

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// SelectReplicaTarget picks a target backend for a replication copy using
// the same routing strategy as normal writes. Excludes backends that
// already hold a copy of the object.
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
	name, err := m.coord.selectBackendForWrite(ctx, size, filtered)
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		return "", nil
	}
	return name, err
}

// -------------------------------------------------------------------------
// PASS-THROUGHS
// -------------------------------------------------------------------------

// DeleteOrEnqueue forwards to the write coordinator. Worker and drain
// interfaces accept *BackendManager and call DeleteOrEnqueue on it; the
// implementation lives on the coordinator now, but the call site shape
// stays the same.
func (m *BackendManager) DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64) {
	m.coord.DeleteOrEnqueue(ctx, be, backendName, key, reason, sizeBytes)
}
