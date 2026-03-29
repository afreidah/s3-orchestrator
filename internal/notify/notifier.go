// -------------------------------------------------------------------------------
// Notifier - Async Webhook Event Delivery
//
// Author: Alex Freidah
//
// Delivers CloudEvents JSON payloads to configured webhook endpoints via HTTP
// POST. Events are persisted in a notification_outbox table for durable retry
// with exponential backoff. A background worker drains the outbox under an
// advisory lock for multi-instance safety. Threshold-crossing events (capacity
// warnings) are dampened to avoid repeated notifications.
// -------------------------------------------------------------------------------

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// OUTBOX STORE INTERFACE
// -------------------------------------------------------------------------

// OutboxStore defines the persistence methods the notifier needs for
// durable event delivery. Implemented by store.Store.
type OutboxStore interface {
	InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error
	GetPendingNotifications(ctx context.Context, limit int) ([]store.NotificationRow, error)
	CompleteNotification(ctx context.Context, id int64) error
	RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// -------------------------------------------------------------------------
// NOTIFIER
// -------------------------------------------------------------------------

// Notifier delivers webhook notifications from a durable outbox queue.
// Implements lifecycle.Service via the Run method.
type Notifier struct {
	endpoints []config.NotificationEndpoint
	store     OutboxStore
	client    *http.Client
	dampener  sync.Map // map[string]time.Time — last-fired per event+subject
}

// NewNotifier creates a notifier backed by the given outbox store. Sets the
// package-level event.Emit hook so all packages can emit notifications via
// the same mechanism.
func NewNotifier(cfg *config.NotificationConfig, store OutboxStore) *Notifier {
	n := &Notifier{
		endpoints: cfg.Endpoints,
		store:     store,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
	event.Emit = n.emit
	return n
}

// -------------------------------------------------------------------------
// EVENT EMISSION
// -------------------------------------------------------------------------

// emit persists an event to the outbox for each matching endpoint. Called
// via the package-level event.Emit hook from any package in the codebase.
func (n *Notifier) emit(ev event.Event) { //nolint:gocritic // Event is passed by value to allow callers to construct inline
	// Fill CloudEvents envelope defaults
	if ev.SpecVersion == "" {
		ev.SpecVersion = "1.0"
	}
	if ev.ID == "" {
		ev.ID = generateEventID()
	}
	if ev.Source == "" {
		ev.Source = "/s3-orchestrator"
	}
	if ev.DataContentType == "" {
		ev.DataContentType = "application/json"
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}

	// Dampening for threshold-crossing events
	if ev.Type == event.BackendCapacityWarning {
		dampenKey := ev.Type + ":" + ev.Subject
		if last, ok := n.dampener.Load(dampenKey); ok {
			if time.Since(last.(time.Time)) < time.Hour {
				telemetry.NotificationDroppedTotal.Inc()
				return
			}
		}
		n.dampener.Store(dampenKey, time.Now())
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		slog.Error("Failed to marshal notification event", "type", ev.Type, "error", err) //nolint:sloglint // emit has no request context
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, ep := range n.endpoints {
		if !event.MatchesFilter(ev.Type, ep.Events) {
			continue
		}
		if ep.Prefix != "" && ev.Subject != "" {
			if !hasPrefix(ev.Subject, ep.Prefix) {
				continue
			}
		}
		if err := n.store.InsertNotification(ctx, ev.Type, string(payload), ep.URL); err != nil {
			slog.ErrorContext(ctx, "Failed to enqueue notification",
				"type", ev.Type, "endpoint", ep.URL, "error", err)
			telemetry.NotificationDroppedTotal.Inc()
		}
	}
}

// -------------------------------------------------------------------------
// BACKGROUND DELIVERY WORKER
// -------------------------------------------------------------------------

// Run implements lifecycle.Service. Drains the notification outbox under an
// advisory lock, delivering pending events via HTTP POST.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n.drainOnce(ctx)
		}
	}
}

// drainOnce processes one batch of pending notifications under an advisory lock.
func (n *Notifier) drainOnce(ctx context.Context) {
	acquired, err := n.store.WithAdvisoryLock(ctx, 1011, func(lockCtx context.Context) error {
		rows, err := n.store.GetPendingNotifications(lockCtx, 50)
		if err != nil {
			return err
		}

		telemetry.NotificationQueueDepth.Set(float64(len(rows)))

		for _, row := range rows {
			ep := n.findEndpoint(row.EndpointURL)
			if ep == nil {
				_ = n.store.CompleteNotification(lockCtx, row.ID)
				continue
			}

			start := time.Now()
			if err := n.deliver(lockCtx, row, ep); err != nil {
				telemetry.NotificationFailedTotal.WithLabelValues(ep.URL, row.EventType).Inc()
				backoff := time.Duration(1<<min(row.Attempts, 6)) * time.Second
				maxRetries := ep.MaxRetries
				if maxRetries <= 0 {
					maxRetries = 3
				}
				if int(row.Attempts)+1 >= maxRetries {
					slog.ErrorContext(lockCtx, "Notification delivery exhausted",
						"endpoint", ep.URL, "event", row.EventType, "attempts", row.Attempts+1, "error", err)
					_ = n.store.CompleteNotification(lockCtx, row.ID)
				} else {
					_ = n.store.RetryNotification(lockCtx, row.ID, backoff, err.Error())
				}
			} else {
				telemetry.NotificationSentTotal.WithLabelValues(ep.URL, row.EventType).Inc()
				telemetry.NotificationDuration.WithLabelValues(ep.URL).Observe(time.Since(start).Seconds())
				_ = n.store.CompleteNotification(lockCtx, row.ID)
			}
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "Notification drain failed", "error", err)
	}
	if !acquired {
		slog.DebugContext(ctx, "Notification drain skipped, another instance holds the lock")
	}
}

// deliver sends a single notification to the endpoint via HTTP POST.
func (n *Notifier) deliver(ctx context.Context, row store.NotificationRow, ep *config.NotificationEndpoint) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(row.Payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")

	if ep.Secret != "" {
		mac := hmac.New(sha256.New, []byte(ep.Secret))
		mac.Write(row.Payload)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Webhook-Signature", "sha256="+sig)
	}

	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	n.client.Timeout = timeout

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", ep.URL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("POST %s: status %d", ep.URL, resp.StatusCode)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// findEndpoint returns the endpoint config matching the given URL, or nil.
func (n *Notifier) findEndpoint(url string) *config.NotificationEndpoint {
	for i := range n.endpoints {
		if n.endpoints[i].URL == url {
			return &n.endpoints[i]
		}
	}
	return nil
}

// hasPrefix checks if a key starts with the given prefix.
func hasPrefix(key, prefix string) bool {
	return len(key) >= len(prefix) && key[:len(prefix)] == prefix
}

// generateEventID creates a random hex event identifier.
func generateEventID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}
