// -------------------------------------------------------------------------------
// Backend State Operations
//
// Author: Alex Freidah
//
// Implements the SQLite engine bindings for backend_states: each backend's
// state flags and its most recent drain's progress. Admission reads the flags
// through backend_capacity. Every flag value the statements use is passed from
// core, so the SQL holds no bit numbers.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ListBackendStates returns every backend state record.
func (s *Store) ListBackendStates(ctx context.Context) ([]core.BackendStateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, flags, objects_moved, last_error, started_at, finished_at
		FROM backend_states
		ORDER BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("list backend states: %w", err)
	}
	return collectRows(rows, "backend state", func(rows *sql.Rows) (core.BackendStateRecord, error) {
		var (
			r          core.BackendStateRecord
			lastError  sql.NullString
			startedAt  string
			finishedAt sql.NullString
		)
		if err := rows.Scan(&r.BackendName, &r.Flags, &r.ObjectsMoved, &lastError, &startedAt, &finishedAt); err != nil {
			return core.BackendStateRecord{}, fmt.Errorf("scan backend state: %w", err)
		}
		started, err := parseTime(startedAt)
		if err != nil {
			return core.BackendStateRecord{}, fmt.Errorf("parse backend state start: %w", err)
		}
		r.LastError = nullStringValue(lastError)
		r.StartedAt = started
		r.FinishedAt = parseNullableTime(finishedAt)
		return r, nil
	})
}

// SetBackendFlag sets one flag on the backend, creating its record when it has
// none. Reports false when the flag was already set.
func (s *Store) SetBackendFlag(ctx context.Context, backendName string, flag core.BackendFlags) (bool, error) {
	return s.execChanged(ctx, "set backend flag", `
		INSERT INTO backend_states (backend_name, flags, started_at)
		VALUES (?1, ?2, ?3)
		ON CONFLICT (backend_name) DO UPDATE
		SET flags = backend_states.flags | ?2
		WHERE (backend_states.flags & ?2) = 0`, backendName, flag, now())
}

// ClearBackendFlags clears the given flags on the backend. Reports false when
// none of them was set.
func (s *Store) ClearBackendFlags(ctx context.Context, backendName string, flags core.BackendFlags) (bool, error) {
	return s.execChanged(ctx, "clear backend flags", `
		UPDATE backend_states SET flags = flags & ~?2
		WHERE backend_name = ?1 AND (flags & ?2) <> 0`, backendName, flags)
}

// StartDrain starts a drain of the backend, or restarts a failed one, keeping
// any other flag such as a cordon. Reports false when the backend is already
// draining or drained.
func (s *Store) StartDrain(ctx context.Context, backendName string) (bool, error) {
	return s.execChanged(ctx, "start drain", `
		INSERT INTO backend_states (backend_name, flags, started_at)
		VALUES (?1, ?2, ?5)
		ON CONFLICT (backend_name) DO UPDATE
		SET flags = (backend_states.flags & ~?3) | ?2,
		    objects_moved = 0, last_error = NULL,
		    started_at = excluded.started_at, finished_at = NULL
		WHERE (backend_states.flags & ?4) = 0`,
		backendName, core.FlagDraining, core.DrainFlags, core.FlagDraining|core.FlagDrained, now())
}

// AddDrainedObjects adds moved to the count of objects a drain in progress has
// moved off its backend.
func (s *Store) AddDrainedObjects(ctx context.Context, backendName string, moved int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE backend_states SET objects_moved = objects_moved + ?
		WHERE backend_name = ? AND (flags & ?) <> 0`, moved, backendName, core.FlagDraining); err != nil {
		return fmt.Errorf("add drained objects: %w", err)
	}
	return nil
}

// MarkDrainFailed records why a drain in progress stopped. The failed flag
// stays, so the backend remains refused until an operator retries or clears
// it.
func (s *Store) MarkDrainFailed(ctx context.Context, backendName, reason string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE backend_states
		SET flags = (flags & ~?3) | ?4, last_error = ?5, finished_at = ?6
		WHERE backend_name = ?1 AND (flags & ?2) <> 0`,
		backendName, core.FlagDraining, core.DrainFlags, core.FlagDrainFailed, reason, now()); err != nil {
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
		UPDATE backend_states SET flags = (flags & ~?3) | ?4, finished_at = ?5
		WHERE backend_name = ?1
		  AND (flags & ?2) <> 0
		  AND NOT EXISTS (SELECT 1 FROM object_locations WHERE backend_name = ?1 AND managed)
		  AND NOT EXISTS (SELECT 1 FROM pending_objects WHERE backend_name = ?1)
		  AND NOT EXISTS (SELECT 1 FROM multipart_uploads WHERE backend_name = ?1)`,
		backendName, core.FlagDraining, core.DrainFlags, core.FlagDrained, now())
}

// ClearBackendState deletes the backend's state record, which removing the
// backend does. Reports whether there was one.
func (s *Store) ClearBackendState(ctx context.Context, backendName string) (bool, error) {
	return s.execChanged(ctx, "clear backend state", `DELETE FROM backend_states WHERE backend_name = ?`, backendName)
}
