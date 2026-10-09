// -------------------------------------------------------------------------------
// Drain Manager - Backend Drain and Remove Operations
//
// Author: Alex Freidah
//
// The operator side of two backend lifecycle operations. A drain is a record in
// the metadata store: starting one writes it, cancelling one clears it, and the
// drainer worker moves the objects off while it is in progress. Remove drops
// every database record for a backend, optionally purging its stored objects,
// and runs synchronously.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// purgePageSize is how many of a backend's objects one purge page removes.
const purgePageSize = 100

// Runtime is the backend-runtime slice the Manager needs: the fleet and the
// delete primitive a purge uses. Defined here at the consumer so
// *infra.BackendRuntime satisfies it structurally without the drain package
// importing infra.
type Runtime interface {
	Backends() map[string]backend.ObjectBackend
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
}

// Progress holds the current state of a drain operation. Active is true while
// the drain is in progress; State is empty for a backend with no drain record.
type Progress struct {
	Active           bool   `json:"active"`
	State            string `json:"state,omitempty"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}

// Manager handles draining and removing backends.
//
// states caches every backend's drain state from the records, so IsDraining
// answers on the write path without a query. It is refreshed whenever this
// manager starts or cancels a drain, on every usage-flush tick, and whenever
// the drainer reads the records. A stale entry only costs a ranking: admission
// reads the records itself.
type Manager struct {
	log              *slog.Logger
	infra            Runtime
	objects          core.ObjectStore
	drains           core.DrainStore
	backendLifecycle core.BackendLifecycleStore
	states           atomic.Pointer[map[string]core.DrainState]
}

// New creates a Manager.
func New(infra Runtime, objects core.ObjectStore, drains core.DrainStore, backendLifecycle core.BackendLifecycleStore) *Manager {
	must.NotNil("infra", infra)
	must.NotNil("objects", objects)
	must.NotNil("drains", drains)
	must.NotNil("backendLifecycle", backendLifecycle)
	return &Manager{
		infra:            infra,
		objects:          objects,
		drains:           drains,
		backendLifecycle: backendLifecycle,
		log:              slog.Default().With(logfmt.Component("drain")),
	}
}

// IsDraining reports whether the named backend has a drain record of any
// state. A drained or failed backend is refused writes as much as one in
// progress.
func (d *Manager) IsDraining(name string) bool {
	_, ok := d.stateOf(name)
	return ok
}

// Refresh reloads the cached drain states from the records.
func (d *Manager) Refresh(ctx context.Context) error {
	drains, err := d.drains.ListDrains(ctx)
	if err != nil {
		return fmt.Errorf("list drains: %w", err)
	}
	d.SetStates(drains)
	return nil
}

// SetStates replaces the cached drain states with the given records. The
// drainer hands it the records each pass reads.
func (d *Manager) SetStates(drains []core.BackendDrain) {
	states := make(map[string]core.DrainState, len(drains))
	for i := range drains {
		states[drains[i].BackendName] = drains[i].State
	}
	d.states.Store(&states)
}

// stateOf returns the cached drain state of the named backend, and false when
// it has no drain record.
func (d *Manager) stateOf(name string) (core.DrainState, bool) {
	states := d.states.Load()
	if states == nil {
		return "", false
	}
	state, ok := (*states)[name]
	return state, ok
}

// -------------------------------------------------------------------------
// DRAIN
// -------------------------------------------------------------------------

// StartDrain records a drain for the backend, or restarts a failed one. From
// that moment admission refuses the backend, and the drainer moves its objects
// off on its next tick.
func (d *Manager) StartDrain(ctx context.Context, name string) error {
	if _, ok := d.infra.Backends()[name]; !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	started, err := d.drains.StartDrain(ctx, name)
	if err != nil {
		return err
	}
	if !started {
		return fmt.Errorf("backend %q is already draining or drained", name)
	}
	d.refreshAfterChange(ctx)
	audit.Log(ctx, "storage.DrainStart", slog.String("backend", name))
	d.log.InfoContext(ctx, "backend drain started", "backend", name)
	return nil
}

// GetDrainProgress returns the state of the backend's drain. Objects and bytes
// remaining are read live while the drain is in progress.
func (d *Manager) GetDrainProgress(ctx context.Context, name string) (*Progress, error) {
	rec, err := d.record(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return &Progress{}, nil
	}
	p := &Progress{
		Active:       rec.State == core.DrainStateDraining,
		State:        string(rec.State),
		ObjectsMoved: rec.ObjectsMoved,
		Error:        rec.LastError,
	}
	if p.Active {
		count, bytes, err := d.backendLifecycle.BackendObjectStats(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get backend stats: %w", err)
		}
		p.ObjectsRemaining = count
		p.BytesRemaining = bytes
	}
	return p, nil
}

// record reads the backend's drain record from the store, bypassing the cache,
// and returns nil when it has none.
func (d *Manager) record(ctx context.Context, name string) (*core.BackendDrain, error) {
	drains, err := d.drains.ListDrains(ctx)
	if err != nil {
		return nil, fmt.Errorf("list drains: %w", err)
	}
	for i := range drains {
		if drains[i].BackendName == name {
			return &drains[i], nil
		}
	}
	return nil, nil
}

// CancelDrain clears the backend's drain record, which makes it writable again.
// A drain in progress stops before its next page; objects already moved are
// not moved back.
func (d *Manager) CancelDrain(ctx context.Context, name string) error {
	cleared, err := d.drains.ClearDrain(ctx, name)
	if err != nil {
		return err
	}
	if !cleared {
		return fmt.Errorf("backend %q is not draining", name)
	}
	d.refreshAfterChange(ctx)
	audit.Log(ctx, "storage.DrainCancel", slog.String("backend", name))
	d.log.InfoContext(ctx, "backend drain cleared", "backend", name)
	return nil
}

// refreshAfterChange reloads the cached states after this manager changed a
// record. A failure is logged and left to the next refresh: the record is
// already written, and admission reads it directly.
func (d *Manager) refreshAfterChange(ctx context.Context) {
	if err := d.Refresh(ctx); err != nil {
		d.log.WarnContext(ctx, "failed to refresh drain states", logfmt.Err(err))
	}
}

// -------------------------------------------------------------------------
// REMOVE
// -------------------------------------------------------------------------

// RemoveBackend deletes all database records for a backend, its drain record
// included. If purge is true and the backend is reachable, also deletes objects
// from the backend's S3 storage. This is destructive and cannot be undone.
// observer, when non-nil, receives a start and end step per object purged.
//
// The drain check reads the record rather than the cache, because a drain
// started or cancelled on another instance may not be cached here yet.
func (d *Manager) RemoveBackend(ctx context.Context, name string, purge bool, observer progress.Observer) error {
	rec, err := d.record(ctx, name)
	if err != nil {
		return err
	}
	if rec != nil && rec.State == core.DrainStateDraining {
		return fmt.Errorf("backend %q is currently draining, cancel the drain first", name)
	}

	ctx = audit.WithRequestID(ctx, audit.NewID())

	if purge {
		if be, ok := d.infra.Backends()[name]; ok {
			d.PurgeBackendObjects(ctx, be, name, observer)
		}
	}

	if err := d.backendLifecycle.DeleteBackendData(ctx, name); err != nil {
		return fmt.Errorf("failed to delete backend data: %w", err)
	}
	if _, err := d.drains.ClearDrain(ctx, name); err != nil {
		return fmt.Errorf("failed to clear drain record: %w", err)
	}
	d.refreshAfterChange(ctx)

	audit.Log(ctx, "storage.RemoveBackend",
		slog.String("backend", name),
		slog.Bool("purge", purge),
	)
	event.Publish(event.BackendRemoved, name, map[string]any{
		"backend": name,
		"purge":   purge,
	})
	d.log.InfoContext(ctx, "backend removed", "backend", name, "purge", purge)

	return nil
}

// PurgeBackendObjects deletes all objects from a backend's S3 storage and their
// metadata rows, a page at a time with one batched backend delete per page.
// Per-key failures are logged and skipped, but a page whose every
// DeleteObjectLocation fails stops the purge, so a persistent DB error cannot
// keep the loop listing and failing on the same rows forever.
func (d *Manager) PurgeBackendObjects(ctx context.Context, be backend.ObjectBackend, name string, observer progress.Observer) {
	for {
		objects, err := d.objects.ListObjectsByBackend(ctx, name, purgePageSize)
		if err != nil {
			d.log.ErrorContext(ctx, "failed to list objects for purge",
				slog.String("backend", name), "error", err)
			return
		}
		if len(objects) == 0 {
			return
		}

		paths := make([]string, len(objects))
		for i := range objects {
			paths[i] = core.StoragePath(objects[i].ObjectKey, objects[i].StorageKey)
		}
		failed := d.infra.DeleteMany(ctx, name, be, paths)

		dbDeleted := 0
		for i := range objects {
			progress.Track(observer, objects[i].ObjectKey, func() string {
				return d.purgeOneObject(ctx, name, &objects[i], failed[paths[i]], &dbDeleted)
			})
		}

		if dbDeleted == 0 {
			d.log.ErrorContext(ctx, "purge made no DB progress on page; bailing to avoid an infinite list-and-fail loop",
				slog.String("backend", name), slog.Int("page_size", len(objects)))
			return
		}
	}
}

// purgeOneObject drops one purged object's metadata row, logging deleteErr when
// its bytes could not be deleted from the backend. Increments dbDeleted on a
// successful DB removal. Returns the progress status: failed when the DB record
// could not be dropped (the signal the page made no progress), ok otherwise.
func (d *Manager) purgeOneObject(ctx context.Context, name string, obj *core.ObjectLocation, deleteErr error, dbDeleted *int) string {
	key := obj.ObjectKey
	if deleteErr != nil {
		d.log.WarnContext(ctx, "failed to delete object from backend during purge",
			slog.String("backend", name), slog.String("key", key), "error", deleteErr)
	}

	_, err := d.objects.DeleteObjectLocation(ctx, key, name)
	if err != nil {
		d.log.WarnContext(ctx, "failed to delete DB record during purge",
			slog.String("backend", name), slog.String("key", key), "error", err)
		return progress.StatusFailed
	}
	*dbDeleted++
	return progress.StatusOK
}
