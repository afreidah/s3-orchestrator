// -------------------------------------------------------------------------------
// SQLite Admin Store - Encryption Admin and Notification Outbox Operations
//
// Author: Alex Freidah
//
// Implements the EncryptionAdmin and NotificationOutbox role interfaces:
// encryption key rotation, encrypt/decrypt existing data operations, and
// the notification outbox used by the event delivery system. These methods
// are not on a request-time role interface and are not wrapped by
// CircuitBreakerStore.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// ENCRYPTION KEY ROTATION
// -------------------------------------------------------------------------

// ListEncryptedLocations returns a page of encrypted object locations filtered
// by key ID, after the cursor. Used during key rotation to find objects wrapped
// with the old key. Paged by cursor because rotating a copy changes its key_id.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit int, after core.Cursor) ([]core.EncryptedLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, encryption_key, key_id
		FROM object_locations
		WHERE encrypted = 1 AND key_id = ?
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		keyID, after.ObjectKey, after.BackendName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	return collectRows(rows, "encrypted locations", func(rows *sql.Rows) (core.EncryptedLocation, error) {
		var loc core.EncryptedLocation
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.EncryptionKey, &loc.KeyID); err != nil {
			return core.EncryptedLocation{}, fmt.Errorf("scan encrypted location: %w", err)
		}
		return loc, nil
	})
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

// ListUnencryptedLocations returns a page of plaintext copies for
// encrypt-existing, on one backend or on all when backend is empty. It pages
// by cursor because encrypting a copy removes it from the set, so an offset
// would skip the rows that moved up.
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.UnencryptedLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes, etag
		FROM object_locations
		WHERE encrypted = 0
		  AND (? = '' OR backend_name = ?)
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		backend, backend, after.ObjectKey, after.BackendName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	return collectRows(rows, "unencrypted locations", func(rows *sql.Rows) (core.UnencryptedLocation, error) {
		var (
			loc  core.UnencryptedLocation
			etag sql.NullString
		)
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes, &etag); err != nil {
			return core.UnencryptedLocation{}, fmt.Errorf("scan unencrypted location: %w", err)
		}
		loc.Etag = nullStringValue(etag)
		return loc, nil
	})
}

// -------------------------------------------------------------------------
// DECRYPT EXISTING
// -------------------------------------------------------------------------

// ListAllEncryptedLocations returns a page of all encrypted object locations
// with decryption metadata. Used by the decrypt-existing admin endpoint.
// Cursor-paged for the same reason as ListUnencryptedLocations: decrypting a
// copy removes it from this set mid-walk.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.DecryptableLocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_key, backend_name, storage_key, size_bytes, encryption_key, key_id, plaintext_size, etag
		FROM object_locations
		WHERE encrypted = 1
		  AND (? = '' OR backend_name = ?)
		  AND (object_key, backend_name) > (?, ?)
		ORDER BY object_key, backend_name
		LIMIT ?`,
		backend, backend, after.ObjectKey, after.BackendName, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list all encrypted locations: %w", err)
	}
	return collectRows(rows, "decryptable locations", func(rows *sql.Rows) (core.DecryptableLocation, error) {
		var (
			loc  core.DecryptableLocation
			etag sql.NullString
		)
		if err := rows.Scan(&loc.ObjectKey, &loc.BackendName, &loc.StorageKey, &loc.SizeBytes, &loc.EncryptionKey, &loc.KeyID, &loc.PlaintextSize, &etag); err != nil {
			return core.DecryptableLocation{}, fmt.Errorf("scan decryptable location: %w", err)
		}
		loc.Etag = nullStringValue(etag)
		return loc, nil
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
func (s *Store) GetPendingNotifications(ctx context.Context, limit int) ([]core.NotificationRow, error) {
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
	return collectRows(rows, "notifications", func(rows *sql.Rows) (core.NotificationRow, error) {
		var n core.NotificationRow
		if err := rows.Scan(&n.ID, &n.EventType, &n.Payload, &n.EndpointURL, &n.Attempts); err != nil {
			return core.NotificationRow{}, fmt.Errorf("scan notification: %w", err)
		}
		return n, nil
	})
}

// NotificationQueueDepth counts the notifications still to be delivered,
// including those waiting out a retry backoff. Rows past the attempt limit are
// never sent again and are not counted.
func (s *Store) NotificationQueueDepth(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_outbox WHERE attempts < 10`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending notifications: %w", err)
	}
	return n, nil
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
	nextRetry := formatTime(time.Now().Add(backoff))
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
