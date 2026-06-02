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

// Package drain owns the backend drain/remove lifecycle. It tracks the
// draining state map, runs the migration goroutine, and exposes IsDraining
// for the proxy core's eligibility filters.
package drain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// Core is the slice of proxy infrastructure the Manager needs. Defined
// here at the consumer so the proxy package can satisfy it structurally
// without exporting a god interface.
type Core interface {
	Backends() map[string]backend.ObjectBackend
	GetBackend(name string) (backend.ObjectBackend, error)
	BackendOrder() []string
	StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
	MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error)
	Acct() *accounting.Recorder
}

// drainState tracks a single in-progress drain operation.
type drainState struct {
	cancel       context.CancelFunc
	done         chan struct{}
	errVal       atomic.Pointer[error] // set on completion; accessed from multiple goroutines
	moved        atomic.Int64          // objects successfully moved
	gaugeDecOnce sync.Once             // guards DrainActive.Dec(); see decrementActiveGauge
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

// decrementActiveGauge fires telemetry.DrainActive.Dec() exactly once
// per drainState across every termination path (success, abort,
// cancel). The sync.Once prevents the cancel-races-completion path
// from decrementing twice when finalizeDrain (or abortDrainWithError)
// fires first and CancelDrain then wakes from <-state.done.
func (s *drainState) decrementActiveGauge() {
	s.gaugeDecOnce.Do(func() { telemetry.DrainActive.Dec() })
}

// Progress holds the current state of a drain operation.
type Progress struct {
	Active           bool   `json:"active"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}

// Manager handles draining and removing backends.
type Manager struct {
	log *slog.Logger
	infra            Core
	objects          core.ObjectStore
	quota            core.QuotaStore
	backendLifecycle core.BackendLifecycleStore

	abortMultipartUploads func(ctx context.Context, backendName string)
	processCleanupQueue   func(ctx context.Context) (processed, failed int)

	draining sync.Map // map[string]*drainState  -  backends being drained
}

// New creates a Manager.
func New(
	infra Core,
	objects core.ObjectStore,
	quota core.QuotaStore,
	backendLifecycle core.BackendLifecycleStore,
	abortMultipartUploads func(ctx context.Context, backendName string),
	processCleanupQueue func(ctx context.Context) (processed, failed int),
) *Manager {
	must.NotNil("infra", infra)
	must.NotNil("objects", objects)
	must.NotNil("quota", quota)
	must.NotNil("backendLifecycle", backendLifecycle)
	must.NotNil("abortMultipartUploads", abortMultipartUploads)
	must.NotNil("processCleanupQueue", processCleanupQueue)
	return &Manager{
		infra:                 infra,
		objects:               objects,
		quota:                 quota,
		backendLifecycle:      backendLifecycle,
		abortMultipartUploads: abortMultipartUploads,
		processCleanupQueue:   processCleanupQueue, log: slog.Default().With(logfmt.Component("drain"))}
}

// IsDraining reports whether the named backend is currently being drained.
func (d *Manager) IsDraining(name string) bool {
	_, ok := d.draining.Load(name)
	return ok
}

// CompletedBackends returns the names of backends whose drain has finished
// (the goroutine closed state.done) and whose state is still in the map.
// FlushUsage uses this to skip backends whose backend_usage rows have
// already been deleted by the drain finalizer.
func (d *Manager) CompletedBackends() map[string]bool {
	completed := make(map[string]bool)
	d.draining.Range(func(key, val any) bool {
		state := val.(*drainState)
		select {
		case <-state.done:
			completed[key.(string)] = true
		default:
		}
		return true
	})
	return completed
}

// ClearState removes all entries from the draining map. Used by tests
// to reset state between runs.
func (d *Manager) ClearState() {
	d.draining.Range(func(key, _ any) bool {
		d.draining.Delete(key)
		return true
	})
}

// noopCancel is the cancel func stored on drainState entries seeded by
// the test helpers. CancelDrain still calls this and waits on done, but
// no real goroutine is running so there is nothing to cancel.
func noopCancel() {
	// Intentionally empty: test-seeded states have no goroutine to stop.
}

// SeedActiveForTest stores an active (not-yet-completed) drain entry for
// the named backend. Lets tests put the manager in the "draining" state
// without launching the full StartDrain goroutine.
func (d *Manager) SeedActiveForTest(name string) {
	d.draining.Store(name, &drainState{
		cancel: noopCancel,
		done:   make(chan struct{}),
	})
}

// SeedCompletedForTest stores a drain-completed entry (done channel
// closed) for the named backend. FlushUsage and the dashboard treat the
// backend as fully drained.
func (d *Manager) SeedCompletedForTest(name string) {
	state := &drainState{
		cancel: noopCancel,
		done:   make(chan struct{}),
	}
	close(state.done)
	d.draining.Store(name, state)
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain begins draining a backend by migrating all objects to other
// backends. The drain runs in a background goroutine. New writes are
// excluded from the draining backend immediately.
func (d *Manager) StartDrain(ctx context.Context, name string) error {
	if _, ok := d.infra.Backends()[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	// Detach cancellation/deadline from the caller's ctx (the drain runs in
	// a background goroutine that must outlive the HTTP request) but
	// preserve its values  -  trace IDs, audit request ID, OTel baggage  -
	// so spans started under drainCtx still link back to the request that
	// initiated the drain. CancelDrain + ctx.Done() in the worker loop
	// provide the explicit shutdown hooks.
	drainCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	state := &drainState{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if _, loaded := d.draining.LoadOrStore(name, state); loaded {
		cancel()
		return fmt.Errorf("backend %q is already draining", name)
	}

	telemetry.DrainActive.Inc()
	d.log.InfoContext(ctx, "starting backend drain", "backend", name)

	go d.runDrain(drainCtx, name, state)
	return nil
}

// GetDrainProgress returns the current state of a drain operation.
func (d *Manager) GetDrainProgress(ctx context.Context, name string) (*Progress, error) {
	val, ok := d.draining.Load(name)
	if !ok {
		return &Progress{Active: false}, nil
	}

	state := val.(*drainState)
	progress := &Progress{
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
	count, bytes, err := d.backendLifecycle.BackendObjectStats(ctx, name)
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
func (d *Manager) CancelDrain(name string) error {
	val, ok := d.draining.Load(name)
	if !ok {
		return fmt.Errorf("backend %q is not draining", name)
	}

	state := val.(*drainState)

	// If already completed, just clear the state.
	select {
	case <-state.done:
		d.draining.Delete(name)
		d.log.InfoContext(context.Background(), "cleared drained state", "backend", name)
		return nil
	default:
	}

	state.cancel()
	<-state.done
	d.draining.Delete(name)
	state.decrementActiveGauge()
	d.log.InfoContext(context.Background(), "cancelled backend drain", "backend", name)
	return nil
}

// runDrain is the background goroutine that migrates objects off a backend.
func (d *Manager) runDrain(ctx context.Context, name string, state *drainState) {
	defer close(state.done)

	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Drain",
		telemetry.AttrOperation.String("drain"),
		telemetry.AttrBackendName.String(name),
	)
	defer span.End()

	d.abortMultipartUploads(ctx, name)
	srcBackend, _ := d.infra.GetBackend(name)

	if err := d.migrateBackendObjects(ctx, name, state, srcBackend); err != nil {
		d.abortDrainWithError(name, state, err)
		return
	}
	d.finalizeDrain(ctx, name, state)
}

// migrateBackendObjects walks the source backend's tracked objects in pages
// and migrates each one. Returns ctx.Err() on cancellation, the store error
// on a list failure, or nil when every page has been processed.
func (d *Manager) migrateBackendObjects(ctx context.Context, name string, state *drainState, srcBackend backend.ObjectBackend) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		objects, err := d.objects.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			d.log.ErrorContext(ctx, "failed to list objects", slog.String("backend", name), "error", err)
			return err
		}
		if len(objects) == 0 {
			return nil
		}

		for i := range objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.DrainOneObject(ctx, srcBackend, name, &objects[i]) {
				state.moved.Add(1)
				telemetry.DrainObjectsMoved.Inc()
				telemetry.DrainBytesMoved.Add(float64(objects[i].SizeBytes))
			}
		}
	}
}

// abortDrainWithError records the failure on state, drops the entry from
// d.draining (so the backend becomes eligible for writes again on next
// reload), and clears the active-drain gauge. Single source of truth for
// the bail-out invariant.
func (d *Manager) abortDrainWithError(name string, state *drainState, err error) {
	state.setErr(err)
	d.draining.Delete(name)
	state.decrementActiveGauge()
}

// finalizeDrain runs after a successful migration. Flushes pending cleanup
// queue items so they are not dropped by the subsequent DeleteBackendData,
// then removes all DB records and clears the active-drain gauge. The entry
// in d.draining stays so the dashboard reports "Drained" and the backend
// remains excluded from new writes until removed from config.
func (d *Manager) finalizeDrain(ctx context.Context, name string, state *drainState) {
	processed, failed := d.processCleanupQueue(ctx)
	if processed > 0 || failed > 0 {
		d.log.InfoContext(ctx, "flushed cleanup queue before removing backend data",
			"backend", name, "processed", processed, "failed", failed)
	}

	if err := d.backendLifecycle.DeleteBackendData(ctx, name); err != nil {
		d.log.ErrorContext(ctx, "failed to clean up backend data", slog.String("backend", name), "error", err)
		state.setErr(err)
	}

	state.decrementActiveGauge()

	audit.Log(ctx, "storage.DrainComplete",
		slog.String("backend", name),
		slog.Int64("objects_moved", state.moved.Load()),
	)
	d.log.InfoContext(ctx, "backend drain complete", "backend", name, "objects_moved", state.moved.Load())
}

// DrainOneObject moves a single object from the draining backend to another.
// If the object already has a replica on another backend, the source copy is
// simply removed (no data transfer needed). Returns true on success.
func (d *Manager) DrainOneObject(ctx context.Context, srcBackend backend.ObjectBackend, srcName string, obj *core.ObjectLocation) bool {
	locations, err := d.objects.GetAllObjectLocations(ctx, obj.ObjectKey)
	if err != nil {
		d.log.WarnContext(ctx, "failed to look up object locations",
			slog.String("key", obj.ObjectKey), "error", err)
		return false
	}
	if other := findOtherBackend(locations, srcName); other != "" {
		return d.removeReplicaSource(ctx, srcBackend, srcName, obj, other)
	}
	return d.copyAndRemoveSource(ctx, srcBackend, srcName, obj)
}

// findOtherBackend returns the first backend in locations that isn't
// srcName, or "" if every location is on the source backend.
func findOtherBackend(locations []core.ObjectLocation, srcName string) string {
	for i := range locations {
		if locations[i].BackendName != srcName {
			return locations[i].BackendName
		}
	}
	return ""
}

// removeReplicaSource handles the fast-path drain branch: a replica
// exists on another backend, so the source-side row and bytes can be
// dropped without a data transfer.
func (d *Manager) removeReplicaSource(ctx context.Context, srcBackend backend.ObjectBackend, srcName string, obj *core.ObjectLocation, replicaBackend string) bool {
	if err := d.objects.DeleteObjectLocation(ctx, obj.ObjectKey, srcName); err != nil {
		d.log.WarnContext(ctx, "failed to delete source location",
			slog.String("key", obj.ObjectKey), slog.String("backend", srcName), "error", err)
		return false
	}
	d.infra.DeleteOrEnqueue(ctx, srcBackend, srcName, obj.ObjectKey, "drain_source_delete", obj.SizeBytes)

	audit.Log(ctx, "storage.DrainRemoveReplica",
		slog.String("key", obj.ObjectKey),
		slog.String("removed_from", srcName),
		slog.String("exists_on", replicaBackend),
	)
	return true
}

// copyAndRemoveSource handles the slow-path drain branch: no replica
// exists, so the object is streamed to a destination backend, the
// metadata location is moved atomically, and the source bytes are then
// deleted. The shared StreamCopy + MoveObjectLocation + cleanup +
// accounting logic lives in writepath.Coordinator.MoveObject;
// this helper picks the destination, calls MoveObject, and emits the
// drain-specific audit on success.
func (d *Manager) copyAndRemoveSource(ctx context.Context, srcBackend backend.ObjectBackend, srcName string, obj *core.ObjectLocation) bool {
	destName, destBackend, ok := d.pickDrainDestination(ctx, srcName, obj)
	if !ok {
		return false
	}

	movedSize, err := d.infra.MoveObject(ctx, &writepath.MoveRequest{
		Key:                obj.ObjectKey,
		SizeBytes:          obj.SizeBytes,
		SrcBackend:         srcBackend,
		SrcName:            srcName,
		DestBackend:        destBackend,
		DestName:           destName,
		OrphanReason:       "drain_orphan",
		StaleOrphanReason:  "drain_stale_orphan",
		SourceDeleteReason: "drain_source_delete",
	})
	if err != nil {
		if !errors.Is(err, writepath.ErrMoveStale) {
			d.log.WarnContext(ctx, "drain move failed",
				slog.String("key", obj.ObjectKey),
				slog.String("src_backend", srcName),
				slog.String("dst_backend", destName),
				"error", err)
		}
		return false
	}

	audit.Log(ctx, "storage.DrainMove",
		slog.String("key", obj.ObjectKey),
		slog.String("src_backend", srcName),
		slog.String("dst_backend", destName),
		slog.Int64("size", movedSize),
	)
	return true
}

// pickDrainDestination chooses the least-utilized non-draining destination
// backend that can accept the object. Returns ok=false (with a logged
// reason) when no destination is reachable.
func (d *Manager) pickDrainDestination(ctx context.Context, srcName string, obj *core.ObjectLocation) (string, backend.ObjectBackend, bool) {
	filtered := make([]string, 0, len(d.infra.BackendOrder()))
	for _, name := range d.infra.BackendOrder() {
		if name == srcName || d.IsDraining(name) {
			continue
		}
		filtered = append(filtered, name)
	}
	destName, err := d.quota.GetLeastUtilizedBackend(ctx, obj.SizeBytes, filtered)
	if err != nil {
		d.log.WarnContext(ctx, "no destination backend available",
			slog.String("key", obj.ObjectKey), slog.Int64("size_bytes", obj.SizeBytes), "error", err)
		return "", nil, false
	}
	destBackend, err := d.infra.GetBackend(destName)
	if err != nil {
		d.log.ErrorContext(ctx, "destination backend not found", "backend", destName)
		return "", nil, false
	}
	return destName, destBackend, true
}

// -------------------------------------------------------------------------
// REMOVE
// -------------------------------------------------------------------------

// RemoveBackend deletes all database records for a backend. If purge is true
// and the backend is reachable, also deletes objects from the backend's S3
// storage. This is destructive and cannot be undone.
func (d *Manager) RemoveBackend(ctx context.Context, name string, purge bool) error {
	if d.IsDraining(name) {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithRequestID(ctx, audit.NewID())

	// Optionally purge objects from the actual S3 backend
	if purge {
		be, ok := d.infra.Backends()[name]
		if ok {
			d.PurgeBackendObjects(ctx, be, name)
		}
	}

	// Delete all DB records in FK-safe order
	if err := d.backendLifecycle.DeleteBackendData(ctx, name); err != nil {
		return fmt.Errorf("failed to delete backend data: %w", err)
	}

	audit.Log(ctx, "storage.RemoveBackend",
		slog.String("backend", name),
		slog.Bool("purge", purge),
	)
	d.log.InfoContext(ctx, "backend removed", "backend", name, "purge", purge)

	return nil
}

// PurgeBackendObjects deletes all objects from a backend's S3 storage
// and their metadata rows. Best-effort: per-key failures are logged
// and the loop continues. Bails on a page whose every DeleteObjectLocation
// fails so a persistent DB error (constraint, partition, conflict)
// cannot pin the loop on the same 100 rows forever; the same rows
// would otherwise list-and-fail until the process was restarted.
func (d *Manager) PurgeBackendObjects(ctx context.Context, be backend.ObjectBackend, name string) {
	for {
		objects, err := d.objects.ListObjectsByBackend(ctx, name, 100)
		if err != nil {
			d.log.ErrorContext(ctx, "failed to list objects for purge",
				slog.String("backend", name), "error", err)
			return
		}
		if len(objects) == 0 {
			return
		}

		dbDeleted := 0
		for i := range objects {
			if err := d.infra.DeleteWithTimeout(ctx, be, objects[i].ObjectKey); err != nil {
				d.log.WarnContext(ctx, "failed to delete object from backend during purge",
					slog.String("backend", name), slog.String("key", objects[i].ObjectKey), "error", err)
			}
			d.infra.Acct().APICall(name)

			if err := d.objects.DeleteObjectLocation(ctx, objects[i].ObjectKey, name); err != nil {
				d.log.WarnContext(ctx, "failed to delete DB record during purge",
					slog.String("backend", name), slog.String("key", objects[i].ObjectKey), "error", err)
				continue
			}
			dbDeleted++
		}

		if dbDeleted == 0 {
			d.log.ErrorContext(ctx, "purge made no DB progress on page; bailing to avoid an infinite list-and-fail loop",
				slog.String("backend", name), slog.Int("page_size", len(objects)))
			return
		}
	}
}
