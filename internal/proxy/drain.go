// -------------------------------------------------------------------------------
// Drain Manager - Backend Drain and Remove Operations
//
// Author: Alex Freidah
//
// Provides two backend lifecycle operations: drain (migrate all objects off a
// backend to other backends, then clean up DB records) and remove (drop all
// DB records for a backend, optionally purging S3 objects). Drain runs as a
// background goroutine; remove is synchronous.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// drainState tracks a single in-progress drain operation.
type drainState struct {
	cancel  context.CancelFunc
	done    chan struct{}
	errVal  atomic.Pointer[error] // set on completion; accessed from multiple goroutines
	moved   atomic.Int64          // objects successfully moved
}

// setErr stores the completion error atomically.
func (s *drainState) setErr(err error) { s.errVal.Store(&err) }

// getErr loads the completion error atomically. Returns nil when unset.
func (s *drainState) getErr() error {
	if p := s.errVal.Load(); p != nil {
		return *p
	}
	return nil
}

// DrainProgress holds the current state of a drain operation.
type DrainProgress struct {
	Active           bool   `json:"active"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}

// DrainManager handles draining and removing backends.
type DrainManager struct {
	*backendCore

	// Cross-component functions injected at construction time.
	abortMultipartUploads func(ctx context.Context, backendName string)
	processCleanupQueue   func(ctx context.Context) (processed, failed int)
}

// NewDrainManager creates a DrainManager sharing the given core infrastructure.
func NewDrainManager(
	core *backendCore,
	abortMultipartUploads func(ctx context.Context, backendName string),
	processCleanupQueue func(ctx context.Context) (processed, failed int),
) *DrainManager {
	return &DrainManager{
		backendCore:           core,
		abortMultipartUploads: abortMultipartUploads,
		processCleanupQueue:   processCleanupQueue,
	}
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain begins draining a backend by migrating all objects to other
// backends. The drain runs in a background goroutine. New writes are
// excluded from the draining backend immediately.
func (d *DrainManager) StartDrain(ctx context.Context, name string) error {
	if _, ok := d.backends[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	drainCtx, cancel := context.WithCancel(context.Background())
	state := &drainState{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if _, loaded := d.draining.LoadOrStore(name, state); loaded {
		cancel()
		return fmt.Errorf("backend %q is already draining", name)
	}

	telemetry.DrainActive.Set(1)
	slog.InfoContext(ctx, "Starting backend drain", "backend", name)

	go d.runDrain(drainCtx, name, state)
	return nil
}

// GetDrainProgress returns the current state of a drain operation.
func (d *DrainManager) GetDrainProgress(ctx context.Context, name string) (*DrainProgress, error) {
	val, ok := d.draining.Load(name)
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
		if err := state.getErr(); err != nil {
			progress.Error = err.Error()
		}
		return progress, nil
	default:
	}

	// Query live stats from DB
	count, bytes, err := d.store.BackendObjectStats(ctx, name)
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
func (d *DrainManager) CancelDrain(name string) error {
	val, ok := d.draining.Load(name)
	if !ok {
		return fmt.Errorf("backend %q is not draining", name)
	}

	state := val.(*drainState)

	// If already completed, just clear the state.
	select {
	case <-state.done:
		d.draining.Delete(name)
		slog.Info("Cleared drained state", "backend", name) //nolint:sloglint // CancelDrain has no context
		return nil
	default:
	}

	state.cancel()
	<-state.done
	d.draining.Delete(name)
	telemetry.DrainActive.Set(0)
	slog.Info("Cancelled backend drain", "backend", name) //nolint:sloglint // CancelDrain has no context
	return nil
}

// runDrain is the background goroutine that migrates objects off a backend.
func (d *DrainManager) runDrain(ctx context.Context, name string, state *drainState) {
	defer close(state.done)

	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Drain",
		telemetry.AttrOperation.String("drain"),
		telemetry.AttrBackendName.String(name),
	)
	defer span.End()

	// Abort in-progress multipart uploads on this backend
	d.abortMultipartUploads(ctx, name)

	srcBackend, _ := d.getBackend(name)

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			state.setErr(ctx.Err())
			d.draining.Delete(name)
			telemetry.DrainActive.Set(0)
			return
		default:
		}

		// Get next batch of objects
		objects, err := d.store.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			slog.ErrorContext(ctx, "Drain: failed to list objects", "backend", name, "error", err)
			state.setErr(err)
			d.draining.Delete(name)
			telemetry.DrainActive.Set(0)
			return
		}

		if len(objects) == 0 {
			break // drain complete
		}

		// Migrate each object
		for i := range objects {
			select {
			case <-ctx.Done():
				state.setErr(ctx.Err())
				d.draining.Delete(name)
				telemetry.DrainActive.Set(0)
				return
			default:
			}

			if d.DrainOneObject(ctx, srcBackend, name, &objects[i]) {
				state.moved.Add(1)
				telemetry.DrainObjectsMoved.Inc()
				telemetry.DrainBytesMoved.Add(float64(objects[i].SizeBytes))
			}
		}
	}

	// Flush any pending cleanup queue items for this backend before
	// deleting all DB records (which would silently drop pending items).
	processed, failed := d.processCleanupQueue(ctx)
	if processed > 0 || failed > 0 {
		slog.InfoContext(ctx, "Drain: flushed cleanup queue before removing backend data",
			"backend", name, "processed", processed, "failed", failed)
	}

	// Drain complete — clean up DB records
	if err := d.store.DeleteBackendData(ctx, name); err != nil {
		slog.ErrorContext(ctx, "Drain: failed to clean up backend data", "backend", name, "error", err)
		state.setErr(err)
	}

	// Keep the entry in d.draining so the dashboard shows "Drained" and
	// the backend remains excluded from new writes until removed from config.
	telemetry.DrainActive.Set(0)

	audit.Log(ctx, "storage.DrainComplete",
		slog.String("backend", name),
		slog.Int64("objects_moved", state.moved.Load()),
	)
	slog.InfoContext(ctx, "Backend drain complete", "backend", name, "objects_moved", state.moved.Load())
}

// drainOneObject moves a single object from the draining backend to another.
// If the object already has a replica on another backend, the source copy is
// simply removed (no data transfer needed). Returns true on success.
func (d *DrainManager) DrainOneObject(ctx context.Context, srcBackend backend.ObjectBackend, srcName string, obj *store.ObjectLocation) bool {
	// Check if the object already has a copy on another backend.
	// If so, just delete the source — no need to copy data.
	locations, err := d.store.GetAllObjectLocations(ctx, obj.ObjectKey)
	if err != nil {
		slog.WarnContext(ctx, "Drain: failed to look up object locations",
			"key", obj.ObjectKey, "error", err)
		return false
	}
	for i := range locations {
		if locations[i].BackendName != srcName {
			// Replica exists elsewhere — delete the source location record
			// and the S3 object. No data transfer required.
			if err := d.store.DeleteObjectLocation(ctx, obj.ObjectKey, srcName); err != nil {
				slog.WarnContext(ctx, "Drain: failed to delete source location",
					"key", obj.ObjectKey, "backend", srcName, "error", err)
				return false
			}
			d.deleteOrEnqueue(ctx, srcBackend, srcName, obj.ObjectKey, "drain_source_delete", obj.SizeBytes)
			d.usage.Record(srcName, 1, 0, 0) // Delete

			audit.Log(ctx, "storage.DrainRemoveReplica",
				slog.String("key", obj.ObjectKey),
				slog.String("removed_from", srcName),
				slog.String("exists_on", locations[i].BackendName),
			)
			return true
		}
	}

	// No replica exists — must copy the object to another backend first.
	eligible := d.excludeDraining(d.order)
	filtered := make([]string, 0, len(eligible))
	for _, name := range eligible {
		if name != srcName {
			filtered = append(filtered, name)
		}
	}

	destName, err := d.store.GetLeastUtilizedBackend(ctx, obj.SizeBytes, filtered)
	if err != nil {
		slog.WarnContext(ctx, "Drain: no destination backend available",
			"key", obj.ObjectKey, "size", obj.SizeBytes, "error", err)
		return false
	}

	destBackend, err := d.getBackend(destName)
	if err != nil {
		slog.ErrorContext(ctx, "Drain: destination backend not found", "backend", destName)
		return false
	}

	// Stream source to destination
	if err := d.streamCopy(ctx, srcBackend, destBackend, obj.ObjectKey); err != nil {
		slog.WarnContext(ctx, "Drain: stream copy failed",
			"key", obj.ObjectKey, "from", srcName, "to", destName, "error", err)
		return false
	}

	// Atomic DB update (compare-and-swap)
	movedSize, err := d.store.MoveObjectLocation(ctx, obj.ObjectKey, srcName, destName)
	if err != nil {
		slog.ErrorContext(ctx, "Drain: failed to update object location",
			"key", obj.ObjectKey, "error", err)
		d.deleteOrEnqueue(ctx, destBackend, destName, obj.ObjectKey, "drain_orphan", obj.SizeBytes)
		d.usage.Record(destName, 1, 0, 0)
		return false
	}

	if movedSize == 0 {
		// Object was deleted or already moved
		d.deleteOrEnqueue(ctx, destBackend, destName, obj.ObjectKey, "drain_stale_orphan", obj.SizeBytes)
		d.usage.Record(destName, 1, 0, 0)
		return false
	}

	// Delete from source
	d.deleteOrEnqueue(ctx, srcBackend, srcName, obj.ObjectKey, "drain_source_delete", movedSize)

	d.usage.Record(srcName, 2, movedSize, 0)  // Get + Delete, egress
	d.usage.Record(destName, 1, 0, movedSize) // Put, ingress

	audit.Log(ctx, "storage.DrainMove",
		slog.String("key", obj.ObjectKey),
		slog.String("from_backend", srcName),
		slog.String("to_backend", destName),
		slog.Int64("size", movedSize),
	)

	return true
}

// -------------------------------------------------------------------------
// REMOVE
// -------------------------------------------------------------------------

// RemoveBackend deletes all database records for a backend. If purge is true
// and the backend is reachable, also deletes objects from the backend's S3
// storage. This is destructive and cannot be undone.
func (d *DrainManager) RemoveBackend(ctx context.Context, name string, purge bool) error {
	if d.IsDraining(name) {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithRequestID(ctx, audit.NewID())

	// Optionally purge objects from the actual S3 backend
	if purge {
		backend, ok := d.backends[name]
		if ok {
			d.PurgeBackendObjects(ctx, backend, name)
		}
	}

	// Delete all DB records in FK-safe order
	if err := d.store.DeleteBackendData(ctx, name); err != nil {
		return fmt.Errorf("failed to delete backend data: %w", err)
	}

	audit.Log(ctx, "storage.RemoveBackend",
		slog.String("backend", name),
		slog.Bool("purge", purge),
	)
	slog.InfoContext(ctx, "Backend removed", "backend", name, "purge", purge)

	return nil
}

// purgeBackendObjects deletes all objects from a backend's S3 storage.
// Best-effort: logs failures but does not stop.
func (d *DrainManager) PurgeBackendObjects(ctx context.Context, backend backend.ObjectBackend, name string) {
	for {
		objects, err := d.store.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			slog.ErrorContext(ctx, "Remove: failed to list objects for purge", "backend", name, "error", err)
			return
		}
		if len(objects) == 0 {
			return
		}

		for i := range objects {
			if err := d.deleteWithTimeout(ctx, backend, objects[i].ObjectKey); err != nil {
				slog.WarnContext(ctx, "Remove: failed to delete object from backend",
					"backend", name, "key", objects[i].ObjectKey, "error", err)
			}
			d.usage.Record(name, 1, 0, 0)

			if err := d.store.DeleteObjectLocation(ctx, objects[i].ObjectKey, name); err != nil {
				slog.WarnContext(ctx, "Remove: failed to delete DB record during purge",
					"backend", name, "key", objects[i].ObjectKey, "error", err)
			}
		}
	}
}
