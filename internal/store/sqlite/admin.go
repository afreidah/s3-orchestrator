// -------------------------------------------------------------------------------
// SQLite Admin Store - Encryption Admin and Notification Outbox Operations
//
// Author: Alex Freidah
//
// Implements AdminStore methods for encryption key rotation, encrypt/decrypt
// existing data operations, and the notification outbox used by the event
// delivery system. These methods are not on the MetadataStore interface and
// are not wrapped by CircuitBreakerStore.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// ENCRYPTION KEY ROTATION
// -------------------------------------------------------------------------

// ListEncryptedLocations returns a page of encrypted object locations filtered
// by key ID. Used during key rotation to find objects wrapped with the old key.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]store.EncryptedLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, encryption_key, key_id
		FROM object_locations
		WHERE encrypted = 1 AND key_id = ?
		ORDER BY object_key, backend_name
		LIMIT ? OFFSET ?`,
		keyID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	defer rows.Close()

	var locs []store.EncryptedLocation
	for rows.Next() {
		var loc store.EncryptedLocation
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.EncryptionKey, &loc.KeyID); err != nil {
			return nil, fmt.Errorf("scan encrypted location: %w", err)
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

// UpdateEncryptionKey updates the wrapped DEK and key ID for a single object
// location. Used after re-wrapping a DEK with a new master key.
func (s *Store) UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE object_locations
		SET encryption_key = ?, key_id = ?
		WHERE object_key = ? AND backend_name = ?`,
		newEncryptionKey, newKeyID, objectKey, backendName,
	)
	if err != nil {
		return fmt.Errorf("update encryption key: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// ENCRYPT EXISTING
// -------------------------------------------------------------------------

// ListUnencryptedLocations returns a page of unencrypted object locations.
// Used by the encrypt-existing admin endpoint to find objects that need
// encryption.
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit, offset int) ([]store.UnencryptedLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes
		FROM object_locations
		WHERE encrypted = 0
		ORDER BY object_key, backend_name
		LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	defer rows.Close()

	var locs []store.UnencryptedLocation
	for rows.Next() {
		var loc store.UnencryptedLocation
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan unencrypted location: %w", err)
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

// MarkObjectEncrypted updates an object location to record that it has been
// encrypted in-place. Updates the size to the ciphertext size and stores the
// encryption metadata.
func (s *Store) MarkObjectEncrypted(ctx context.Context, objectKey, backendName string, encryptionKey []byte, keyID string, plaintextSize, ciphertextSize int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE object_locations
			SET encrypted = 1, encryption_key = ?, key_id = ?,
			    plaintext_size = ?, size_bytes = ?
			WHERE object_key = ? AND backend_name = ?`,
			encryptionKey, keyID, plaintextSize, ciphertextSize, objectKey, backendName,
		)
		if err != nil {
			return fmt.Errorf("mark encrypted: %w", err)
		}

		sizeDelta := ciphertextSize - plaintextSize
		if sizeDelta != 0 {
			_, err = tx.ExecContext(ctx, `
				UPDATE backend_quotas
				SET bytes_used = bytes_used + ?, updated_at = ?
				WHERE backend_name = ?`,
				sizeDelta, now(), backendName,
			)
			if err != nil {
				return fmt.Errorf("adjust quota for encryption: %w", err)
			}
		}
		return nil
	})
}

// -------------------------------------------------------------------------
// DECRYPT EXISTING
// -------------------------------------------------------------------------

// ListAllEncryptedLocations returns a page of all encrypted object locations
// with decryption metadata. Used by the decrypt-existing admin endpoint.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit, offset int) ([]store.DecryptableLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, size_bytes, encryption_key, key_id, plaintext_size
		FROM object_locations
		WHERE encrypted = 1
		ORDER BY object_key, backend_name
		LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list all encrypted locations: %w", err)
	}
	defer rows.Close()

	var locs []store.DecryptableLocation
	for rows.Next() {
		var loc store.DecryptableLocation
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.SizeBytes, &loc.EncryptionKey, &loc.KeyID, &loc.PlaintextSize); err != nil {
			return nil, fmt.Errorf("scan decryptable location: %w", err)
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

// MarkObjectDecrypted updates an object location to record that it has been
// decrypted in-place. Clears encryption metadata and restores the plaintext
// size.
func (s *Store) MarkObjectDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var currentSize int64
		err := tx.QueryRowContext(ctx, `
			SELECT size_bytes FROM object_locations
			WHERE object_key = ? AND backend_name = ?`,
			objectKey, backendName,
		).Scan(&currentSize)
		if err != nil {
			return fmt.Errorf("read current size: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE object_locations
			SET encrypted = 0, encryption_key = NULL, key_id = NULL,
			    plaintext_size = NULL, size_bytes = ?
			WHERE object_key = ? AND backend_name = ?`,
			plaintextSize, objectKey, backendName,
		)
		if err != nil {
			return fmt.Errorf("mark decrypted: %w", err)
		}

		sizeDelta := plaintextSize - currentSize
		if sizeDelta != 0 {
			_, err = tx.ExecContext(ctx, `
				UPDATE backend_quotas
				SET bytes_used = bytes_used + ?, updated_at = ?
				WHERE backend_name = ?`,
				sizeDelta, now(), backendName,
			)
			if err != nil {
				return fmt.Errorf("adjust quota for decryption: %w", err)
			}
		}
		return nil
	})
}

// -------------------------------------------------------------------------
// NOTIFICATION OUTBOX
// -------------------------------------------------------------------------

// InsertNotification enqueues a notification for delivery.
func (s *Store) InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_outbox (event_type, payload, endpoint_url, created_at, next_retry, attempts)
		VALUES (?, ?, ?, ?, ?, 0)`,
		eventType, payload, endpointURL, now(), now(),
	)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

// GetPendingNotifications returns notifications ready for delivery, ordered
// by creation time.
func (s *Store) GetPendingNotifications(ctx context.Context, limit int) ([]store.NotificationRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event_type, payload, endpoint_url, attempts
		FROM notification_outbox
		WHERE next_retry <= ? AND attempts < 10
		ORDER BY created_at ASC
		LIMIT ?`,
		now(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending notifications: %w", err)
	}
	defer rows.Close()

	var notifs []store.NotificationRow
	for rows.Next() {
		var n store.NotificationRow
		if err := rows.Scan(&n.ID, &n.EventType, &n.Payload, &n.EndpointURL, &n.Attempts); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		notifs = append(notifs, n)
	}
	return notifs, rows.Err()
}

// CompleteNotification removes a successfully delivered notification from the
// outbox.
func (s *Store) CompleteNotification(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notification_outbox WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("complete notification: %w", err)
	}
	return nil
}

// RetryNotification increments the attempt counter and schedules the next
// retry at an absolute time computed from the backoff duration.
func (s *Store) RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	nextRetry := time.Now().Add(backoff).UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		UPDATE notification_outbox
		SET attempts = attempts + 1, next_retry = ?, last_error = ?
		WHERE id = ?`,
		nextRetry, lastError, id,
	)
	if err != nil {
		return fmt.Errorf("retry notification: %w", err)
	}
	return nil
}
