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
// by key ID. Used during key rotation to find objects wrapped with the old key.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]core.EncryptedLocation, error) {
	rows, err := s.queries.ListEncryptedLocations(ctx, db.ListEncryptedLocationsParams{
		KeyID:  &keyID,
		Limit:  int32(limit),  //nolint:gosec // G115: limit is a small caller-controlled batch size
		Offset: int32(offset), //nolint:gosec // G115: offset is a small caller-controlled value
	})
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	return mapSlice(rows, encryptedLocationFromRow), nil
}

// encryptedLocationFromRow converts a sqlc ListEncryptedLocations row
// to core.EncryptedLocation, safely dereferencing the nullable KeyID.
func encryptedLocationFromRow(r *db.ListEncryptedLocationsRow) core.EncryptedLocation {
	return core.EncryptedLocation{
		ObjectKey:     r.ObjectKey,
		BackendName:   r.BackendName,
		EncryptionKey: r.EncryptionKey,
		KeyID:         derefStr(r.KeyID),
	}
}

// UpdateEncryptionKey re-wraps a single object's encryption key. Used during
// key rotation to replace the old wrapped DEK with one wrapped by the new key.
func (s *Store) UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error {
	return s.queries.UpdateEncryptionKey(ctx, db.UpdateEncryptionKeyParams{
		ObjectKey:     objectKey,
		BackendName:   backendName,
		EncryptionKey: newEncryptionKey,
		KeyID:         &newKeyID,
	})
}

// ListUnencryptedLocations returns a page of unencrypted object locations.
// Used by the encrypt-existing admin endpoint to find objects that need encryption.
// CountUnencryptedLocations reports how many copies are still stored as
// plaintext. Enabling encryption only affects new writes, so this is what says
// whether a fleet is actually covered or merely configured to be.
func (s *Store) CountUnencryptedLocations(ctx context.Context) (int64, error) {
	n, err := s.queries.CountUnencryptedLocations(ctx)
	if err != nil {
		return 0, fmt.Errorf("count unencrypted locations: %w", err)
	}
	return n, nil
}

func (s *Store) ListUnencryptedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.UnencryptedLocation, error) {
	rows, err := s.queries.ListUnencryptedLocations(ctx, db.ListUnencryptedLocationsParams{
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	return mapSlice(rows, unencryptedLocationFromRow), nil
}

// unencryptedLocationFromRow converts a sqlc ListUnencryptedLocations
// row to core.UnencryptedLocation.
func unencryptedLocationFromRow(r *db.ListUnencryptedLocationsRow) core.UnencryptedLocation {
	return core.UnencryptedLocation{
		ObjectKey:   r.ObjectKey,
		BackendName: r.BackendName,
		SizeBytes:   r.SizeBytes,
	}
}

// ListAllEncryptedLocations returns a page of all encrypted object locations.
// Used by the decrypt-existing admin endpoint to find objects that need decryption.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit int, after core.Cursor) ([]core.DecryptableLocation, error) {
	rows, err := s.queries.ListAllEncryptedLocations(ctx, db.ListAllEncryptedLocationsParams{
		AfterKey:     after.ObjectKey,
		AfterBackend: after.BackendName,
		RowLimit:     int32(limit), //nolint:gosec // G115: limit is a small caller-controlled batch size
	})
	if err != nil {
		return nil, fmt.Errorf("list all encrypted locations: %w", err)
	}
	return mapSlice(rows, decryptableLocationFromRow), nil
}

// decryptableLocationFromRow converts a sqlc ListAllEncryptedLocations
// row to core.DecryptableLocation, safely dereferencing nullable
// KeyID and PlaintextSize.
func decryptableLocationFromRow(r *db.ListAllEncryptedLocationsRow) core.DecryptableLocation {
	return core.DecryptableLocation{
		ObjectKey:     r.ObjectKey,
		BackendName:   r.BackendName,
		SizeBytes:     r.SizeBytes,
		EncryptionKey: r.EncryptionKey,
		KeyID:         derefStr(r.KeyID),
		PlaintextSize: derefInt64(r.PlaintextSize),
	}
}

// -------------------------------------------------------------------------
// STORED-FORM REWRITES
//
// MarkObjectEncrypted, MarkObjectDecrypted and MarkObjectCompressed are
// promoted operations: their whole body is a core transaction over the
// TxAdapter, so they live in core.TxOps and this engine contributes only the
// statements behind it.
// -------------------------------------------------------------------------
