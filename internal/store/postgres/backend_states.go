// -------------------------------------------------------------------------------
// Backend State Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_states, the durable record
// of each backend's state, such as a drain. Admission refuses a backend with a
// record by reading the same table through backend_capacity.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// StartDrain records a drain for the backend, or restarts a failed one. Reports
// false when the backend is already draining or drained.
func (s *Store) StartDrain(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.StartDrain(ctx, backendName)
	if err != nil {
		return false, fmt.Errorf("start drain: %w", err)
	}
	return n > 0, nil
}

// ListBackendStates returns every backend state record, whatever its state.
func (s *Store) ListBackendStates(ctx context.Context) ([]core.BackendStateRecord, error) {
	rows, err := s.queries.ListBackendStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list backend states: %w", err)
	}
	out := make([]core.BackendStateRecord, len(rows))
	for i := range rows {
		out[i] = backendStateFromRow(&rows[i])
	}
	return out, nil
}

// AddDrainedObjects adds moved to the count of objects a drain in progress has
// moved off its backend.
func (s *Store) AddDrainedObjects(ctx context.Context, backendName string, moved int64) error {
	if err := s.queries.AddDrainedObjects(ctx, db.AddDrainedObjectsParams{
		Moved: moved, BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("add drained objects: %w", err)
	}
	return nil
}

// MarkDrainFailed records why a drain in progress stopped. The record stays, so
// the backend remains refused until an operator retries or clears it.
func (s *Store) MarkDrainFailed(ctx context.Context, backendName, reason string) error {
	if err := s.queries.MarkDrainFailed(ctx, db.MarkDrainFailedParams{
		Reason: reason, BackendName: backendName,
	}); err != nil {
		return fmt.Errorf("mark drain failed: %w", err)
	}
	return nil
}

// CompleteDrain marks a drain in progress as drained when no managed object
// rows, intents, or multipart uploads remain on the backend. Reports false when
// something remains, which leaves the drain in progress.
func (s *Store) CompleteDrain(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.CompleteDrain(ctx, backendName)
	if err != nil {
		return false, fmt.Errorf("complete drain: %w", err)
	}
	return n > 0, nil
}

// ClearBackendState deletes the backend's state record, which makes it
// writable again. Reports whether there was one.
func (s *Store) ClearBackendState(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.ClearBackendState(ctx, backendName)
	if err != nil {
		return false, fmt.Errorf("clear backend state: %w", err)
	}
	return n > 0, nil
}

// backendStateFromRow converts a sqlc backend_states row to the core type.
func backendStateFromRow(r *db.BackendState) core.BackendStateRecord {
	return core.BackendStateRecord{
		BackendName:  r.BackendName,
		State:        core.BackendState(r.State),
		ObjectsMoved: r.ObjectsMoved,
		LastError:    derefStr(r.LastError),
		StartedAt:    r.StartedAt.Time,
		FinishedAt:   timestamptzPtr(r.FinishedAt),
	}
}
