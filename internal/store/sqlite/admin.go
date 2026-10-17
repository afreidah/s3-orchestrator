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
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	return s.listCopies(ctx, "encrypted locations", `encrypted = 1 AND key_id = ?`, limit, after, "", keyID)
}

// -------------------------------------------------------------------------
// ENCRYPT EXISTING
// -------------------------------------------------------------------------

// ListUnencryptedLocations returns a page of plaintext copies for
// encrypt-existing, on one backend or on all when backend is empty. It pages
// by cursor because encrypting a copy removes it from the set, so an offset
// would skip the rows that moved up.
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	return s.listCopies(ctx, "unencrypted locations", `encrypted = 0`, limit, after, backend)
}

// -------------------------------------------------------------------------
// DECRYPT EXISTING
// -------------------------------------------------------------------------

// ListAllEncryptedLocations returns a page of all encrypted object locations
// with decryption metadata. Used by the decrypt-existing admin endpoint.
// Cursor-paged for the same reason as ListUnencryptedLocations: decrypting a
// copy removes it from this set mid-walk.
func (s *Store) ListAllEncryptedLocations(ctx context.Context, limit int, after core.Cursor, backend string) ([]core.ObjectLocation, error) {
	return s.listCopies(ctx, "decryptable locations", `encrypted = 1`, limit, after, backend)
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
