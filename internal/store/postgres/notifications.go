// -------------------------------------------------------------------------------
// Notification Outbox Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the notification_outbox
// table that backs durable webhook delivery: synchronous inserts at
// every event site, fetch-pending with backoff-aware scheduling,
// complete on successful delivery, and retry with exponential backoff
// on failure. The outbox pattern lets the API path return success the
// instant the row is committed, with the actual HTTP delivery handled
// asynchronously by a background drainer.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// InsertNotification adds an event to the notification outbox for async
// webhook delivery. Satisfies the notify.OutboxStore interface.
func (s *Store) InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error {
	return s.queries.InsertNotification(ctx, db.InsertNotificationParams{
		EventType:   eventType,
		Payload:     []byte(payload),
		EndpointUrl: endpointURL,
	})
}

// GetPendingNotifications returns outbox rows ready for delivery.
func (s *Store) GetPendingNotifications(ctx context.Context, limit int) ([]core.NotificationRow, error) {
	rows, err := s.queries.GetPendingNotifications(ctx, int32(limit)) //nolint:gosec // G115: limit is a small caller-controlled batch size
	if err != nil {
		return nil, err
	}
	out := make([]core.NotificationRow, len(rows))
	for i, r := range rows {
		out[i] = core.NotificationRow{
			ID:          r.ID,
			EventType:   r.EventType,
			Payload:     r.Payload,
			EndpointURL: r.EndpointUrl,
			Attempts:    r.Attempts,
		}
	}
	return out, nil
}

// CompleteNotification removes a delivered notification from the outbox.
// Satisfies the notify.OutboxStore interface.
func (s *Store) CompleteNotification(ctx context.Context, id int64) error {
	return s.queries.CompleteNotification(ctx, id)
}

// RetryNotification increments the attempt counter and schedules the next
// retry. Satisfies the notify.OutboxStore interface.
func (s *Store) RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	return s.queries.RetryNotification(ctx, db.RetryNotificationParams{
		ID:        id,
		Backoff:   pgtype.Interval{Microseconds: backoff.Microseconds(), Valid: true},
		LastError: &lastError,
	})
}
