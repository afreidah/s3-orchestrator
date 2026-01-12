// -------------------------------------------------------------------------------
// Store - PostgreSQL Quota and Object Location Storage
//
// Project: Munchbox / Author: Alex Freidah
//
// Manages quota tracking and object location storage in PostgreSQL. Tracks which
// backend stores each object and how much quota each backend has used. Provides
// atomic operations to ensure quota limits are respected.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// -------------------------------------------------------------------------
// ERRORS
// -------------------------------------------------------------------------

var (
	// ErrNoSpaceAvailable is returned when no backend has enough quota.
	ErrNoSpaceAvailable = errors.New("no backend has sufficient quota")

	// ErrObjectNotFound is returned when an object is not in the location table.
	ErrObjectNotFound = errors.New("object not found")
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Store manages quota and object location data in PostgreSQL.
type Store struct {
	db *sql.DB
}

// QuotaStat holds quota statistics for a single backend.
type QuotaStat struct {
	BackendName string
	BytesUsed   int64
	BytesLimit  int64
	UpdatedAt   time.Time
}

// ObjectLocation holds information about where an object is stored.
type ObjectLocation struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
	CreatedAt   time.Time
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewStore creates a new PostgreSQL store connection.
func NewStore(connStr string) (*Store, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// -------------------------------------------------------------------------
// QUOTA OPERATIONS
// -------------------------------------------------------------------------

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []BackendConfig) error {
	for _, b := range backends {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO backend_quotas (backend_name, bytes_limit, bytes_used, updated_at)
			VALUES ($1, $2, 0, NOW())
			ON CONFLICT (backend_name) DO UPDATE SET
				bytes_limit = $2,
				updated_at = NOW()
		`, b.Name, b.QuotaBytes)

		if err != nil {
			return fmt.Errorf("failed to sync quota for backend %s: %w", b.Name, err)
		}
	}
	return nil
}

// GetBackendWithSpace finds a backend with enough quota for the given size.
// Returns the backend name or ErrNoSpaceAvailable if none have enough space.
func (s *Store) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	// Query backends in the configured order
	for _, name := range backendOrder {
		var available int64
		err := s.db.QueryRowContext(ctx, `
			SELECT bytes_limit - bytes_used
			FROM backend_quotas
			WHERE backend_name = $1
		`, name).Scan(&available)

		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to check quota for %s: %w", name, err)
		}

		if available >= size {
			return name, nil
		}
	}

	return "", ErrNoSpaceAvailable
}

// GetQuotaStats returns quota statistics for all backends.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, bytes_used, bytes_limit, updated_at
		FROM backend_quotas
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[string]QuotaStat)
	for rows.Next() {
		var stat QuotaStat
		if err := rows.Scan(&stat.BackendName, &stat.BytesUsed, &stat.BytesLimit, &stat.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan quota row: %w", err)
		}
		stats[stat.BackendName] = stat
	}

	return stats, rows.Err()
}

// -------------------------------------------------------------------------
// OBJECT LOCATION OPERATIONS
// -------------------------------------------------------------------------

// RecordObject records an object's location and updates the backend quota.
// This is done atomically in a transaction.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check if object already exists (overwrite case)
	var existingBackend string
	var existingSize int64
	err = tx.QueryRowContext(ctx, `
		SELECT backend_name, size_bytes FROM object_locations WHERE object_key = $1
	`, key).Scan(&existingBackend, &existingSize)

	if err == nil {
		// Object exists - remove old quota usage
		_, err = tx.ExecContext(ctx, `
			UPDATE backend_quotas
			SET bytes_used = bytes_used - $1, updated_at = NOW()
			WHERE backend_name = $2
		`, existingSize, existingBackend)
		if err != nil {
			return fmt.Errorf("failed to decrement old quota: %w", err)
		}

		// Update location to new backend
		_, err = tx.ExecContext(ctx, `
			UPDATE object_locations
			SET backend_name = $1, size_bytes = $2, created_at = NOW()
			WHERE object_key = $3
		`, backend, size, key)
		if err != nil {
			return fmt.Errorf("failed to update object location: %w", err)
		}
	} else if err == sql.ErrNoRows {
		// New object - insert location
		_, err = tx.ExecContext(ctx, `
			INSERT INTO object_locations (object_key, backend_name, size_bytes, created_at)
			VALUES ($1, $2, $3, NOW())
		`, key, backend, size)
		if err != nil {
			return fmt.Errorf("failed to insert object location: %w", err)
		}
	} else {
		return fmt.Errorf("failed to check existing object: %w", err)
	}

	// Update quota for new backend
	_, err = tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = bytes_used + $1, updated_at = NOW()
		WHERE backend_name = $2
	`, size, backend)
	if err != nil {
		return fmt.Errorf("failed to update quota: %w", err)
	}

	return tx.Commit()
}

// GetObjectLocation finds which backend stores the given object.
func (s *Store) GetObjectLocation(ctx context.Context, key string) (*ObjectLocation, error) {
	var loc ObjectLocation
	err := s.db.QueryRowContext(ctx, `
		SELECT object_key, backend_name, size_bytes, created_at
		FROM object_locations
		WHERE object_key = $1
	`, key).Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &loc.CreatedAt)

	if err == sql.ErrNoRows {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get object location: %w", err)
	}

	return &loc, nil
}

// DeleteObject removes an object's location record and decrements the quota.
// Returns the backend name and size for the deleted object, or ErrObjectNotFound.
func (s *Store) DeleteObject(ctx context.Context, key string) (backend string, size int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get object info
	err = tx.QueryRowContext(ctx, `
		SELECT backend_name, size_bytes FROM object_locations WHERE object_key = $1
	`, key).Scan(&backend, &size)

	if err == sql.ErrNoRows {
		return "", 0, ErrObjectNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("failed to get object location: %w", err)
	}

	// Delete location record
	_, err = tx.ExecContext(ctx, `
		DELETE FROM object_locations WHERE object_key = $1
	`, key)
	if err != nil {
		return "", 0, fmt.Errorf("failed to delete object location: %w", err)
	}

	// Decrement quota
	_, err = tx.ExecContext(ctx, `
		UPDATE backend_quotas
		SET bytes_used = bytes_used - $1, updated_at = NOW()
		WHERE backend_name = $2
	`, size, backend)
	if err != nil {
		return "", 0, fmt.Errorf("failed to decrement quota: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("failed to commit: %w", err)
	}

	return backend, size, nil
}
