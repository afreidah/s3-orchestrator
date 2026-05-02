// -------------------------------------------------------------------------------
// Admin and Backend Lifecycle Operations
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"fmt"

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

// DeleteObjectLocation removes a single object_locations row for the given key
// and backend. Used by drain to remove source copies when a replica exists.
func (s *Store) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	return s.queries.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
		ObjectKey:   key,
		BackendName: backendName,
	})
}

// -------------------------------------------------------------------------
// KEY ROTATION (admin-only, not on MetadataStore interface)
// -------------------------------------------------------------------------

// ListEncryptedLocations returns a page of encrypted object locations filtered
// by key ID. Used during key rotation to find objects wrapped with the old key.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]EncryptedLocation, error) {
	rows, err := s.queries.ListEncryptedLocations(ctx, db.ListEncryptedLocationsParams{
		KeyID:  &keyID,
		Limit:  int32(limit),  //nolint:gosec // G115: limit is a small caller-controlled batch size
		Offset: int32(offset), //nolint:gosec // G115: offset is a small caller-controlled value
	})
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	result := make([]EncryptedLocation, len(rows))
	for i, r := range rows {
		result[i] = EncryptedLocation{
			ObjectKey:     r.ObjectKey,
			BackendName:   r.BackendName,
			EncryptionKey: r.EncryptionKey,
		}
		if r.KeyID != nil {
			result[i].KeyID = *r.KeyID
		}
	}
	return result, nil
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
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit, offset int) ([]UnencryptedLocation, error) {
	rows, err := s.queries.ListUnencryptedLocations(ctx, db.ListUnencryptedLocationsParams{
		Limit:  int32(limit),  //nolint:gosec // G115: limit is a small caller-controlled batch size
		Offset: int32(offset), //nolint:gosec // G115: offset is a small caller-controlled value
	})
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	result := make([]UnencryptedLocation, len(rows))
	for i, r := range rows {
		result[i] = UnencryptedLocation{
			ObjectKey:   r.ObjectKey,
			BackendName: r.BackendName,
			SizeBytes:   r.SizeBytes,
		}
	}
	return result, nil
}

// MarkObjectEncrypted updates a single object location to record that it has
// been encrypted. Sets the encryption flag, wrapped DEK, key ID, plaintext
// size, and updates size_bytes to the ciphertext size.
func (s *Store) MarkObjectEncrypted(ctx context.Context, objectKey, backendName string, encryptionKey []byte, keyID string, plaintextSize, ciphertextSize int64) error {
	return s.queries.MarkObjectEncrypted(ctx, db.MarkObjectEncryptedParams{
		ObjectKey:     objectKey,
		BackendName:   backendName,
		EncryptionKey: encryptionKey,
		KeyID:         &keyID,
		PlaintextSize: &plaintextSize,
		SizeBytes:     ciphertextSize,
	})
}

// ListAllEncryptedLocations returns a page of all encrypted object locations.
// Used by the decrypt-existing admin endpoint to find objects that need decryption.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit, offset int) ([]DecryptableLocation, error) {
	rows, err := s.queries.ListAllEncryptedLocations(ctx, db.ListAllEncryptedLocationsParams{
		Limit:  int32(limit),  //nolint:gosec // G115: limit is a small caller-controlled batch size
		Offset: int32(offset), //nolint:gosec // G115: offset is a small caller-controlled value
	})
	if err != nil {
		return nil, fmt.Errorf("list all encrypted locations: %w", err)
	}
	result := make([]DecryptableLocation, len(rows))
	for i, r := range rows {
		result[i] = DecryptableLocation{
			ObjectKey:     r.ObjectKey,
			BackendName:   r.BackendName,
			SizeBytes:     r.SizeBytes,
			EncryptionKey: r.EncryptionKey,
		}
		if r.KeyID != nil {
			result[i].KeyID = *r.KeyID
		}
		if r.PlaintextSize != nil {
			result[i].PlaintextSize = *r.PlaintextSize
		}
	}
	return result, nil
}

// MarkObjectDecrypted updates a single object location to record that it has
// been decrypted. Clears the encryption flag, wrapped DEK, key ID, and
// plaintext size, and updates size_bytes to the plaintext size.
func (s *Store) MarkObjectDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error {
	return s.queries.MarkObjectDecrypted(ctx, db.MarkObjectDecryptedParams{
		ObjectKey:   objectKey,
		BackendName: backendName,
		SizeBytes:   plaintextSize,
	})
}
