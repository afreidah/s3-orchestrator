// -------------------------------------------------------------------------------
// Backend State Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_states: each backend's
// state flags and its most recent drain's progress. Admission reads the flags
// through backend_capacity. Every flag value the queries use is passed from
// core, so the SQL holds no bit numbers.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// ListBackendStates returns every backend state record.
func (s *Store) ListBackendStates(ctx context.Context) ([]core.BackendStateRecord, error) {
	rows, err := s.queries.ListBackendStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list backend states: %w", err)
	}
	return mapSlice(rows, backendStateFromRow), nil
}

// SetBackendFlag sets one flag on the backend, creating its record when it has
// none. Reports false when the flag was already set.
func (s *Store) SetBackendFlag(ctx context.Context, backendName string, flag core.BackendFlags) (bool, error) {
	n, err := s.queries.SetBackendFlag(ctx, db.SetBackendFlagParams{
		BackendName: backendName, Flag: int64(flag),
	})
	return changed(n, err, "set backend flag")
}

// ClearBackendFlags clears the given flags on the backend. Reports false when
// none of them was set.
func (s *Store) ClearBackendFlags(ctx context.Context, backendName string, flags core.BackendFlags) (bool, error) {
	n, err := s.queries.ClearBackendFlags(ctx, db.ClearBackendFlagsParams{
		BackendName: backendName, ClearFlags: int64(flags),
	})
	return changed(n, err, "clear backend flags")
}

// StartDrain starts a drain of the backend, or restarts a failed one, keeping
// any other flag such as a cordon. Reports false when the backend is already
// draining or drained.
func (s *Store) StartDrain(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.StartDrain(ctx, db.StartDrainParams{
		BackendName: backendName,
		Draining:    int64(core.FlagDraining),
		DrainFlags:  int64(core.DrainFlags),
		Busy:        int64(core.FlagDraining | core.FlagDrained),
	})
	return changed(n, err, "start drain")
}

// AddDrainedObjects adds moved to the count of objects a drain in progress has
// moved off its backend.
func (s *Store) AddDrainedObjects(ctx context.Context, backendName string, moved int64) error {
	if err := s.queries.AddDrainedObjects(ctx, db.AddDrainedObjectsParams{
		Moved: moved, BackendName: backendName, Draining: int64(core.FlagDraining),
	}); err != nil {
		return fmt.Errorf("add drained objects: %w", err)
	}
	return nil
}

// MarkDrainFailed records why a drain in progress stopped. The failed flag
// stays, so the backend remains refused until an operator retries or clears
// it.
func (s *Store) MarkDrainFailed(ctx context.Context, backendName, reason string) error {
	if err := s.queries.MarkDrainFailed(ctx, db.MarkDrainFailedParams{
		Reason:      reason,
		BackendName: backendName,
		Draining:    int64(core.FlagDraining),
		DrainFlags:  int64(core.DrainFlags),
		Failed:      int64(core.FlagDrainFailed),
	}); err != nil {
		return fmt.Errorf("mark drain failed: %w", err)
	}
	return nil
}

// CompleteDrain marks a drain in progress as drained when no managed object
// rows, intents, or multipart uploads remain on the backend. Reports false when
// something remains, which leaves the drain in progress.
func (s *Store) CompleteDrain(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.CompleteDrain(ctx, db.CompleteDrainParams{
		BackendName: backendName,
		Draining:    int64(core.FlagDraining),
		DrainFlags:  int64(core.DrainFlags),
		Drained:     int64(core.FlagDrained),
	})
	return changed(n, err, "complete drain")
}

// ClearBackendState deletes the backend's state record, which removing the
// backend does. Reports whether there was one.
func (s *Store) ClearBackendState(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.ClearBackendState(ctx, backendName)
	return changed(n, err, "clear backend state")
}

// backendStateFromRow converts a sqlc backend_states row to the core type.
func backendStateFromRow(r *db.ListBackendStatesRow) core.BackendStateRecord {
	return core.BackendStateRecord{
		BackendName:  r.BackendName,
		Flags:        core.BackendFlags(r.Flags),
		ObjectsMoved: r.ObjectsMoved,
		LastError:    derefStr(r.LastError),
		StartedAt:    r.StartedAt.Time,
		FinishedAt:   timestamptzPtr(r.FinishedAt),
	}
}
