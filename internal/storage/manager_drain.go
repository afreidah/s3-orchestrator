// -------------------------------------------------------------------------------
// Backend Drain and Remove Operations
//
// Author: Alex Freidah
//
// Provides two backend lifecycle operations: drain (migrate all objects off a
// backend to other backends, then clean up DB records) and remove (drop all
// DB records for a backend, optionally purging S3 objects). Drain runs as a
// background goroutine; remove is synchronous.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/audit"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// drainState tracks a single in-progress drain operation.
type drainState struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error        // set on completion
	moved  atomic.Int64 // objects successfully moved
}

// DrainProgress holds the current state of a drain operation.
type DrainProgress struct {
	Active          bool   `json:"active"`
	ObjectsRemaining int64 `json:"objects_remaining"`
	BytesRemaining  int64  `json:"bytes_remaining"`
	ObjectsMoved    int64  `json:"objects_moved"`
	Error           string `json:"error,omitempty"`
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain begins draining a backend by migrating all objects to other
// backends. The drain runs in a background goroutine. New writes are
// excluded from the draining backend immediately.
func (m *BackendManager) StartDrain(ctx context.Context, name string) error {
	if _, ok := m.backends[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	if _, loaded := m.draining.LoadOrStore(name, nil); loaded {
		return fmt.Errorf("backend %q is already draining", name)
	}

	drainCtx, cancel := context.WithCancel(context.Background())
	state := &drainState{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.draining.Store(name, state)

	telemetry.DrainActive.Set(1)
	slog.Info("Starting backend drain", "backend", name)

	go m.runDrain(drainCtx, name, state)
	return nil
}

// GetDrainProgress returns the current state of a drain operation.
func (m *BackendManager) GetDrainProgress(ctx context.Context, name string) (*DrainProgress, error) {
	val, ok := m.draining.Load(name)
	if !ok {
		return &DrainProgress{Active: false}, nil
	}

	state := val.(*drainState)
	progress := &DrainProgress{
		Active:       true,
		ObjectsMoved: state.moved.Load(),
	}

	// Check if drain has completed
	select {
	case <-state.done:
		progress.Active = false
		if state.err != nil {
			progress.Error = state.err.Error()
		}
		return progress, nil
	default:
	}

	// Query live stats from DB
	count, bytes, err := m.store.BackendObjectStats(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get backend stats: %w", err)
	}
	progress.ObjectsRemaining = count
	progress.BytesRemaining = bytes

	return progress, nil
}

// CancelDrain stops an active drain operation. If the drain has already
// completed, it clears the "drained" state so the backend becomes eligible
// for writes again. Objects already moved are not rolled back.
func (m *BackendManager) CancelDrain(name string) error {
	val, ok := m.draining.Load(name)
	if !ok {
		return fmt.Errorf("backend %q is not draining", name)
	}

	state := val.(*drainState)

	// If already completed, just clear the state.
	select {
	case <-state.done:
		m.draining.Delete(name)
		slog.Info("Cleared drained state", "backend", name)
		return nil
	default:
	}

	state.cancel()
	<-state.done
	m.draining.Delete(name)
	telemetry.DrainActive.Set(0)
	slog.Info("Cancelled backend drain", "backend", name)
	return nil
}

// runDrain is the background goroutine that migrates objects off a backend.
func (m *BackendManager) runDrain(ctx context.Context, name string, state *drainState) {
	defer close(state.done)

	ctx = audit.WithRequestID(ctx, audit.NewID())

	// Abort in-progress multipart uploads on this backend
	m.abortMultipartUploadsOnBackend(ctx, name)

	srcBackend, _ := m.getBackend(name)

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			state.err = ctx.Err()
			m.draining.Delete(name)
			telemetry.DrainActive.Set(0)
			return
		default:
		}

		// Get next batch of objects
		objects, err := m.store.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			slog.Error("Drain: failed to list objects", "backend", name, "error", err)
			state.err = err
			m.draining.Delete(name)
			telemetry.DrainActive.Set(0)
			return
		}

		if len(objects) == 0 {
			break // drain complete
		}

		// Migrate each object
		for _, obj := range objects {
			select {
			case <-ctx.Done():
				state.err = ctx.Err()
				m.draining.Delete(name)
				telemetry.DrainActive.Set(0)
				return
			default:
			}

			if m.drainOneObject(ctx, srcBackend, name, obj) {
				state.moved.Add(1)
				telemetry.DrainObjectsMoved.Inc()
				telemetry.DrainBytesMoved.Add(float64(obj.SizeBytes))
			}
		}
	}

	// Drain complete — clean up DB records
	if err := m.store.DeleteBackendData(ctx, name); err != nil {
		slog.Error("Drain: failed to clean up backend data", "backend", name, "error", err)
		state.err = err
	}

	// Keep the entry in m.draining so the dashboard shows "Drained" and
	// the backend remains excluded from new writes until removed from config.
	telemetry.DrainActive.Set(0)

	audit.Log(ctx, "storage.DrainComplete",
		slog.String("backend", name),
		slog.Int64("objects_moved", state.moved.Load()),
	)
	slog.Info("Backend drain complete", "backend", name, "objects_moved", state.moved.Load())
}

// drainOneObject moves a single object from the draining backend to another.
// If the object already has a replica on another backend, the source copy is
// simply removed (no data transfer needed). Returns true on success.
func (m *BackendManager) drainOneObject(ctx context.Context, srcBackend ObjectBackend, srcName string, obj ObjectLocation) bool {
	// Check if the object already has a copy on another backend.
	// If so, just delete the source — no need to copy data.
	locations, err := m.store.GetAllObjectLocations(ctx, obj.ObjectKey)
	if err != nil {
		slog.Warn("Drain: failed to look up object locations",
			"key", obj.ObjectKey, "error", err)
		return false
	}
	for _, loc := range locations {
		if loc.BackendName != srcName {
			// Replica exists elsewhere — delete the source location record
			// and the S3 object. No data transfer required.
			if err := m.store.DeleteObjectLocation(ctx, obj.ObjectKey, srcName); err != nil {
				slog.Warn("Drain: failed to delete source location",
					"key", obj.ObjectKey, "backend", srcName, "error", err)
				return false
			}
			m.deleteOrEnqueue(ctx, srcBackend, srcName, obj.ObjectKey, "drain_source_delete")
			m.usage.Record(srcName, 1, 0, 0) // Delete

			audit.Log(ctx, "storage.DrainRemoveReplica",
				slog.String("key", obj.ObjectKey),
				slog.String("removed_from", srcName),
				slog.String("exists_on", loc.BackendName),
			)
			return true
		}
	}

	// No replica exists — must copy the object to another backend first.
	eligible := m.excludeDraining(m.order)
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if name != srcName {
			filtered = append(filtered, name)
		}
	}

	destName, err := m.store.GetLeastUtilizedBackend(ctx, obj.SizeBytes, filtered)
	if err != nil {
		slog.Warn("Drain: no destination backend available",
			"key", obj.ObjectKey, "size", obj.SizeBytes, "error", err)
		return false
	}

	destBackend, err := m.getBackend(destName)
	if err != nil {
		slog.Error("Drain: destination backend not found", "backend", destName)
		return false
	}

	// Read from source
	rctx, rcancel := m.withTimeout(ctx)
	result, err := srcBackend.GetObject(rctx, obj.ObjectKey, "")
	if err != nil {
		rcancel()
		slog.Warn("Drain: failed to read source object",
			"key", obj.ObjectKey, "backend", srcName, "error", err)
		return false
	}

	// Write to destination
	wctx, wcancel := m.withTimeout(ctx)
	_, err = destBackend.PutObject(wctx, obj.ObjectKey, result.Body, result.Size, result.ContentType)
	_ = result.Body.Close()
	rcancel()
	wcancel()
	if err != nil {
		slog.Warn("Drain: failed to write destination object",
			"key", obj.ObjectKey, "backend", destName, "error", err)
		return false
	}

	// Atomic DB update (compare-and-swap)
	movedSize, err := m.store.MoveObjectLocation(ctx, obj.ObjectKey, srcName, destName)
	if err != nil {
		slog.Error("Drain: failed to update object location",
			"key", obj.ObjectKey, "error", err)
		m.deleteOrEnqueue(ctx, destBackend, destName, obj.ObjectKey, "drain_orphan")
		return false
	}

	if movedSize == 0 {
		// Object was deleted or already moved
		m.deleteOrEnqueue(ctx, destBackend, destName, obj.ObjectKey, "drain_stale_orphan")
		return false
	}

	// Delete from source
	m.deleteOrEnqueue(ctx, srcBackend, srcName, obj.ObjectKey, "drain_source_delete")

	m.usage.Record(srcName, 2, movedSize, 0) // Get + Delete, egress
	m.usage.Record(destName, 1, 0, movedSize) // Put, ingress

	audit.Log(ctx, "storage.DrainMove",
		slog.String("key", obj.ObjectKey),
		slog.String("from_backend", srcName),
		slog.String("to_backend", destName),
		slog.Int64("size", movedSize),
	)

	return true
}

// abortMultipartUploadsOnBackend aborts all in-progress multipart uploads
// on the given backend.
func (m *BackendManager) abortMultipartUploadsOnBackend(ctx context.Context, backendName string) {
	uploads, err := m.store.GetStaleMultipartUploads(ctx, 0)
	if err != nil {
		slog.Error("Drain: failed to list multipart uploads", "error", err)
		return
	}

	for _, mu := range uploads {
		if mu.BackendName != backendName {
			continue
		}
		slog.Info("Drain: aborting multipart upload", "upload_id", mu.UploadID, "key", mu.ObjectKey)
		if err := m.AbortMultipartUpload(ctx, mu.UploadID); err != nil {
			slog.Error("Drain: failed to abort multipart upload",
				"upload_id", mu.UploadID, "error", err)
		}
	}
}

// -------------------------------------------------------------------------
// REMOVE
// -------------------------------------------------------------------------

// RemoveBackend deletes all database records for a backend. If purge is true
// and the backend is reachable, also deletes objects from the backend's S3
// storage. This is destructive and cannot be undone.
func (m *BackendManager) RemoveBackend(ctx context.Context, name string, purge bool) error {
	if m.IsDraining(name) {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithRequestID(ctx, audit.NewID())

	// Optionally purge objects from the actual S3 backend
	if purge {
		backend, ok := m.backends[name]
		if ok {
			m.purgeBackendObjects(ctx, backend, name)
		}
	}

	// Delete all DB records in FK-safe order
	if err := m.store.DeleteBackendData(ctx, name); err != nil {
		return fmt.Errorf("failed to delete backend data: %w", err)
	}

	audit.Log(ctx, "storage.RemoveBackend",
		slog.String("backend", name),
		slog.Bool("purge", purge),
	)
	slog.Info("Backend removed", "backend", name, "purge", purge)

	return nil
}

// purgeBackendObjects deletes all objects from a backend's S3 storage.
// Best-effort: logs failures but does not stop.
func (m *BackendManager) purgeBackendObjects(ctx context.Context, backend ObjectBackend, name string) {
	for {
		objects, err := m.store.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			slog.Error("Remove: failed to list objects for purge", "backend", name, "error", err)
			return
		}
		if len(objects) == 0 {
			return
		}

		for _, obj := range objects {
			dctx, dcancel := m.withTimeout(ctx)
			if err := backend.DeleteObject(dctx, obj.ObjectKey); err != nil {
				slog.Warn("Remove: failed to delete object from backend",
					"backend", name, "key", obj.ObjectKey, "error", err)
			}
			dcancel()
		}
	}
}
