// -------------------------------------------------------------------------------
// Backend State Operations
//
// Author: Alex Freidah
//
// Implements the SQLite engine bindings for backend_states, the durable record
// of each backend's state, such as a drain. Admission refuses a backend with a
// record by reading the same table through backend_capacity.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// StartDrain records a drain for the backend, or restarts a failed one. Reports
// false when the backend is already draining or drained.
func (s *Store) StartDrain(ctx context.Context, backendName string) (bool, error) {
	return s.execChanged(ctx, "start drain", `
		INSERT INTO backend_states (backend_name, state, started_at)
		VALUES (?, 'draining', ?)
		ON CONFLICT (backend_name) DO UPDATE
		SET state = 'draining', objects_moved = 0, last_error = NULL,
		    started_at = excluded.started_at, finished_at = NULL
		WHERE backend_states.state = 'failed'`, backendName, now())
}

// ListBackendStates returns every backend state record, whatever its state.
func (s *Store) ListBackendStates(ctx context.Context) ([]core.BackendStateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, state, objects_moved, last_error, started_at, finished_at
		FROM backend_states
		ORDER BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("list backend states: %w", err)
	}
	return collectRows(rows, "backend state", func(rows *sql.Rows) (core.BackendStateRecord, error) {
		var (
			r          core.BackendStateRecord
			state      string
			lastError  sql.NullString
			startedAt  string
			finishedAt sql.NullString
		)
		if err := rows.Scan(&r.BackendName, &state, &r.ObjectsMoved, &lastError, &startedAt, &finishedAt); err != nil {
			return core.BackendStateRecord{}, fmt.Errorf("scan backend state: %w", err)
		}
		started, err := parseTime(startedAt)
		if err != nil {
			return core.BackendStateRecord{}, fmt.Errorf("parse backend state start: %w", err)
		}
		r.State = core.BackendState(state)
		r.LastError = nullStringValue(lastError)
		r.StartedAt = started
		r.FinishedAt = parseNullableTime(finishedAt)
		return r, nil
	})
}

// AddDrainedObjects adds moved to the count of objects a drain in progress has
// moved off its backend.
func (s *Store) AddDrainedObjects(ctx context.Context, backendName string, moved int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE backend_states SET objects_moved = objects_moved + ?
		WHERE backend_name = ? AND state = 'draining'`, moved, backendName); err != nil {
		return fmt.Errorf("add drained objects: %w", err)
	}
	return nil
}

// MarkDrainFailed records why a drain in progress stopped. The record stays, so
// the backend remains refused until an operator retries or clears it.
func (s *Store) MarkDrainFailed(ctx context.Context, backendName, reason string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE backend_states SET state = 'failed', last_error = ?, finished_at = ?
		WHERE backend_name = ? AND state = 'draining'`,
		reason, now(), backendName); err != nil {
		return fmt.Errorf("mark drain failed: %w", err)
	}
	return nil
}

// CompleteDrain marks a drain in progress as drained when no managed object
// rows, intents, or multipart uploads remain on the backend. Unmanaged rows are
// not counted: a drain leaves them, and removing the backend deletes them.
// Reports false when something remains, which leaves the drain in progress.
func (s *Store) CompleteDrain(ctx context.Context, backendName string) (bool, error) {
	return s.execChanged(ctx, "complete drain", `
		UPDATE backend_states SET state = 'drained', finished_at = ?2
		WHERE backend_name = ?1
		  AND state = 'draining'
		  AND NOT EXISTS (SELECT 1 FROM object_locations WHERE backend_name = ?1 AND managed)
		  AND NOT EXISTS (SELECT 1 FROM pending_objects WHERE backend_name = ?1)
		  AND NOT EXISTS (SELECT 1 FROM multipart_uploads WHERE backend_name = ?1)`,
		backendName, now())
}

// ClearBackendState deletes the backend's state record, which makes it
// writable again. Reports whether there was one.
func (s *Store) ClearBackendState(ctx context.Context, backendName string) (bool, error) {
	return s.execChanged(ctx, "clear backend state", `DELETE FROM backend_states WHERE backend_name = ?`, backendName)
}
