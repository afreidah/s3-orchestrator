// -------------------------------------------------------------------------------
// Admin and Backend Lifecycle Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for backend-lifecycle ops:
// adding a backend to backend_quotas at startup (UpsertQuotaLimit),
// listing backends, and deleting all metadata for a removed backend
// (DeleteBackendData). Translates between sqlc-generated row types
// and the canonical core domain types so business logic in core/
// stays engine-agnostic.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE
// -------------------------------------------------------------------------

// BackendObjectStats returns the object count and total bytes stored on a backend.
func (s *Store) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	row, err := s.queries.BackendObjectStats(ctx, backendName)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get backend object stats: %w", err)
	}
	return row.ObjectCount, row.TotalBytes, nil
}

// DeleteBackendData removes all database records for a backend in FK-safe order.
// Runs in a single transaction.
func (s *Store) DeleteBackendData(ctx context.Context, backendName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.queries.WithTx(tx)

	if err := qtx.DeleteCleanupQueueByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete cleanup queue: %w", err)
	}
	if err := qtx.DeleteMultipartUploadsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete multipart uploads: %w", err)
	}
	if err := qtx.DeleteObjectLocationsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete object locations: %w", err)
	}
	if err := qtx.DeleteUsageByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete usage records: %w", err)
	}
	if err := qtx.DeleteQuota(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete quota: %w", err)
	}

	return tx.Commit(ctx)
}

// -------------------------------------------------------------------------
// KEY ROTATION (admin-only, not on MetadataStore interface)
// -------------------------------------------------------------------------

// ListEncryptedLocations returns a page of encrypted object locations filtered
// by key ID, after the cursor. Used during key rotation to find objects wrapped
// with the old key.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListEncryptedLocations(ctx, db.ListEncryptedLocationsParams{
		KeyID:        keyID,
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	return toIdentifiedObjectLocations(rows), nil
}

// ListUnencryptedLocations returns a page of unencrypted object locations.
// Used by the encrypt-existing admin endpoint to find objects that need encryption.
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListUnencryptedLocations(ctx, db.ListUnencryptedLocationsParams{
		BackendFilter: backend,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	return toIdentifiedObjectLocations(rows), nil
}

// ListAllEncryptedLocations returns a page of all encrypted object locations.
// Used by the decrypt-existing admin endpoint to find objects that need decryption.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	rows, err := s.queries.ListAllEncryptedLocations(ctx, db.ListAllEncryptedLocationsParams{
		BackendFilter: backend,
		AfterKey:      after.ObjectKey,
		AfterBackend:  after.BackendName,
		RowLimit:      int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list all encrypted locations: %w", err)
	}
	return toIdentifiedObjectLocations(rows), nil
}

// -------------------------------------------------------------------------
// STORED-FORM REWRITES
//
// MarkObjectEncrypted, MarkObjectDecrypted and MarkObjectCompressed are
// promoted operations: their whole body is a core transaction over the
// TxAdapter, so they live in core.TxOps and this engine contributes only the
// statements behind it.
// -------------------------------------------------------------------------
