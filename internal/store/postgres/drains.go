// -------------------------------------------------------------------------------
// Backend Drain Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend_drains, the durable record
// of each backend's drain. Admission refuses a drained backend by reading the
// same table through backend_capacity.
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

// ListDrains returns every drain record, whatever its state.
func (s *Store) ListDrains(ctx context.Context) ([]core.BackendDrain, error) {
	rows, err := s.queries.ListDrains(ctx)
	if err != nil {
		return nil, fmt.Errorf("list drains: %w", err)
	}
	out := make([]core.BackendDrain, len(rows))
	for i := range rows {
		out[i] = backendDrainFromRow(&rows[i])
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

// ClearDrain deletes the backend's drain record, which makes it writable again.
// Reports whether there was one.
func (s *Store) ClearDrain(ctx context.Context, backendName string) (bool, error) {
	n, err := s.queries.ClearDrain(ctx, backendName)
	if err != nil {
		return false, fmt.Errorf("clear drain: %w", err)
	}
	return n > 0, nil
}

// backendDrainFromRow converts a sqlc backend_drains row to the core type.
func backendDrainFromRow(r *db.BackendDrain) core.BackendDrain {
	return core.BackendDrain{
		BackendName:  r.BackendName,
		State:        core.DrainState(r.State),
		ObjectsMoved: r.ObjectsMoved,
		LastError:    derefStr(r.LastError),
		StartedAt:    r.StartedAt.Time,
		FinishedAt:   timestamptzPtr(r.FinishedAt),
	}
}
