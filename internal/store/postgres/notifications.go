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

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// InsertNotification adds an event to the notification outbox for async
// webhook delivery. Satisfies the notify.OutboxStore interface.
func (s *Store) InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error {
	return wrapErr(s.queries.InsertNotification(ctx, db.InsertNotificationParams{
		EventType:   eventType,
		Payload:     []byte(payload),
		EndpointUrl: endpointURL,
	}), "insert notification")
}

// GetPendingNotifications returns outbox rows ready for delivery.
func (s *Store) GetPendingNotifications(ctx context.Context, limit int) ([]core.NotificationRow, error) {
	rows, err := s.queries.GetPendingNotifications(ctx, rowLimit(limit))
	if err != nil {
		return nil, wrapErr(err, "get pending notifications")
	}
	return mapSlice(rows, notificationFromRow), nil
}

// notificationFromRow converts a sqlc GetPendingNotifications row to the
// core.NotificationRow shape consumed by the notify worker.
func notificationFromRow(r *db.GetPendingNotificationsRow) core.NotificationRow {
	return core.NotificationRow{
		ID:          r.ID,
		EventType:   r.EventType,
		Payload:     r.Payload,
		EndpointURL: r.EndpointUrl,
		Attempts:    r.Attempts,
	}
}

// NotificationQueueDepth counts the notifications still to be delivered,
// including those waiting out a retry backoff. The notifier deletes a row once
// its endpoint's retries are spent, so every row still counts.
func (s *Store) NotificationQueueDepth(ctx context.Context) (int64, error) {
	n, err := s.queries.NotificationQueueDepth(ctx)
	return n, wrapErr(err, "failed to count pending notifications")
}

// CompleteNotification removes a delivered notification from the outbox.
// Satisfies the notify.OutboxStore interface.
func (s *Store) CompleteNotification(ctx context.Context, id int64) error {
	return wrapErr(s.queries.CompleteNotification(ctx, id), "complete notification")
}

// RetryNotification increments the attempt counter and schedules the next
// retry. Satisfies the notify.OutboxStore interface.
func (s *Store) RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error {
	return wrapErr(s.queries.RetryNotification(ctx, db.RetryNotificationParams{
		ID:        id,
		Backoff:   durationToInterval(backoff),
		LastError: &lastError,
	}), "retry notification")
}
