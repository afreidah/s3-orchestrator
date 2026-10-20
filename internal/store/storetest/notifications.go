// -------------------------------------------------------------------------------
// Notification Outbox Attempts Check
//
// Author: Alex Freidah
//
// Each endpoint sets how many times a notification is retried, and the
// notifier deletes a row once those retries are spent. The outbox queries
// therefore apply no attempt limit of their own: a row stays due and counted
// however many attempts it has made.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// notificationAttempts is an attempt count past ten, which an endpoint with a
// high max_retries reaches.
const notificationAttempts = 12

// CheckNotificationAttempts inserts a notification, retries it twelve times
// with a backoff already elapsed, and fails unless it is still pending and
// still counted in the queue depth. The store may hold other rows; the row
// is removed when the test ends.
func CheckNotificationAttempts(t *testing.T, s core.NotificationOutbox) {
	t.Helper()
	ctx := context.Background()
	url := "https://example.invalid/" + t.Name()
	before, err := s.NotificationQueueDepth(ctx)
	if err != nil {
		t.Fatalf("NotificationQueueDepth: %v", err)
	}
	if err := s.InsertNotification(ctx, "attempts.event", `{}`, url); err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}
	id := pendingNotification(t, s, url).ID
	t.Cleanup(func() { _ = s.CompleteNotification(context.Background(), id) })

	for range notificationAttempts {
		if err := s.RetryNotification(ctx, id, -time.Second, "failed"); err != nil {
			t.Fatalf("RetryNotification: %v", err)
		}
	}

	if got := pendingNotification(t, s, url).Attempts; got != notificationAttempts {
		t.Errorf("attempts = %d, want %d", got, notificationAttempts)
	}
	if got, err := s.NotificationQueueDepth(ctx); err != nil || got != before+1 {
		t.Errorf("depth = %d, %v; want %d", got, err, before+1)
	}
}

// pendingNotification returns the due notification bound for url, failing
// the test when there is none.
func pendingNotification(t *testing.T, s core.NotificationOutbox, url string) core.NotificationRow {
	t.Helper()
	pending, err := s.GetPendingNotifications(context.Background(), 1000)
	if err != nil {
		t.Fatalf("GetPendingNotifications: %v", err)
	}
	for _, p := range pending {
		if p.EndpointURL == url {
			return p
		}
	}
	t.Fatalf("no pending notification for %s", url)
	return core.NotificationRow{}
}
