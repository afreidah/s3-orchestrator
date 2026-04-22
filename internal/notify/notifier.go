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
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
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

// dampenTTL is the duration for which repeated threshold-crossing events
// (e.g. capacity warnings) are suppressed after the first emission.
const dampenTTL = 1 * time.Hour

// httpClientTimeout is the default per-request HTTP timeout the shared
// http.Client is initialized with. Per-endpoint overrides (ep.Timeout)
// replace this on a per-delivery basis.
const httpClientTimeout = 10 * time.Second

// emitTimeout bounds the outbox-write fan-out inside a single emit() call
// when an event targets multiple endpoints. Must cover all endpoint inserts
// combined, not each one.
const emitTimeout = 5 * time.Second

// drainInterval controls how often the background delivery worker wakes
// to check for pending notifications.
const drainInterval = 2 * time.Second

// pendingBatchSize caps how many pending rows are pulled from the outbox
// per drain tick. Bounds memory use if notifications back up.
const pendingBatchSize = 50

// advisoryLockKey is the PostgreSQL advisory-lock key the notifier uses to
// serialize outbox drains across multiple replicas.
const advisoryLockKey = 1011

// maxBackoffShift bounds the exponential-backoff shift so we cap at 2^6 = 64
// seconds between retries, regardless of attempt count.
const maxBackoffShift = 6

// defaultMaxRetries is used when an endpoint doesn't set MaxRetries (or sets
// it <= 0). After this many failed attempts, a notification is dropped.
const defaultMaxRetries = 3

// defaultEndpointTimeout is used when an endpoint doesn't set Timeout (or
// sets it <= 0).
const defaultEndpointTimeout = 5 * time.Second

// Notifier delivers webhook notifications from a durable outbox queue.
// Implements lifecycle.Service via the Run method.
type Notifier struct {
	endpoints []config.NotificationEndpoint
	store     OutboxStore
	client    *http.Client
	dampener  *syncutil.TTLCache[string, struct{}]
}

// NewNotifier creates a notifier backed by the given outbox store. Sets the
// package-level event.Emit hook so all packages can emit notifications via
// the same mechanism.
func NewNotifier(cfg *config.NotificationConfig, store OutboxStore) *Notifier {
	n := &Notifier{
		endpoints: cfg.Endpoints,
		store:     store,
		client: &http.Client{
			Timeout: httpClientTimeout,
		},
		dampener: syncutil.NewTTLCache[string, struct{}](dampenTTL),
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

	// Dampening for threshold-crossing events. The TTL cache automatically
	// evicts entries after dampenTTL, preventing unbounded memory growth.
	if ev.Type == event.BackendCapacityWarning && n.dampener != nil {
		dampenKey := ev.Type + ":" + ev.Subject
		if _, ok := n.dampener.Get(dampenKey); ok {
			telemetry.NotificationDroppedTotal.Inc()
			return
		}
		n.dampener.Set(dampenKey, struct{}{})
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		slog.Error("Failed to marshal notification event", "type", ev.Type, "error", err) //nolint:sloglint // emit has no request context
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
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
			slog.ErrorContext(ctx, "failed to enqueue notification",
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
	ticker := time.NewTicker(drainInterval)
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
	acquired, err := n.store.WithAdvisoryLock(ctx, advisoryLockKey, func(lockCtx context.Context) error {
		rows, err := n.store.GetPendingNotifications(lockCtx, pendingBatchSize)
		if err != nil {
			return err
		}

		telemetry.NotificationQueueDepth.Set(float64(len(rows)))

		for _, row := range rows {
			ep := n.findEndpoint(row.EndpointURL)
			if ep == nil {
				n.completeOrLog(lockCtx, row.ID, "no_matching_endpoint")
				continue
			}

			start := time.Now()
			if err := n.deliver(lockCtx, row, ep); err != nil {
				telemetry.NotificationFailedTotal.WithLabelValues(ep.URL, row.EventType).Inc()
				backoff := time.Duration(1<<min(row.Attempts, maxBackoffShift)) * time.Second
				maxRetries := ep.MaxRetries
				if maxRetries <= 0 {
					maxRetries = defaultMaxRetries
				}
				if int(row.Attempts)+1 >= maxRetries {
					slog.ErrorContext(lockCtx, "notification delivery exhausted",
						"endpoint", ep.URL, "event", row.EventType, "attempts", row.Attempts+1, "error", err)
					n.completeOrLog(lockCtx, row.ID, "exhausted")
				} else {
					n.retryOrLog(lockCtx, row.ID, backoff, err.Error())
				}
			} else {
				telemetry.NotificationSentTotal.WithLabelValues(ep.URL, row.EventType).Inc()
				telemetry.NotificationDuration.WithLabelValues(ep.URL).Observe(time.Since(start).Seconds())
				n.completeOrLog(lockCtx, row.ID, "delivered")
			}
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "notification drain failed", "error", err)
	}
	if !acquired {
		slog.DebugContext(ctx, "notification drain skipped, another instance holds the lock")
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
		timeout = defaultEndpointTimeout
	}
	n.client.Timeout = timeout

	resp, err := n.client.Do(req) //nolint:gosec // G704: endpoint URL is operator-configured, not user-tainted
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

// completeOrLog marks a notification row complete in the outbox. Store
// errors are logged at WARN and counted in the notification-store-errors
// metric rather than silently discarded, since a dropped Complete can lead
// to a webhook being delivered twice on the next drain tick.
func (n *Notifier) completeOrLog(ctx context.Context, id int64, reason string) {
	if err := n.store.CompleteNotification(ctx, id); err != nil {
		telemetry.NotificationStoreErrorsTotal.WithLabelValues("complete").Inc()
		slog.WarnContext(ctx, "Notifier: CompleteNotification failed",
			"id", id, "reason", reason, "error", err)
	}
}

// retryOrLog schedules a notification for retry. Store errors are logged
// and counted (see completeOrLog) rather than silently discarded.
func (n *Notifier) retryOrLog(ctx context.Context, id int64, backoff time.Duration, reason string) {
	if err := n.store.RetryNotification(ctx, id, backoff, reason); err != nil {
		telemetry.NotificationStoreErrorsTotal.WithLabelValues("retry").Inc()
		slog.WarnContext(ctx, "Notifier: RetryNotification failed",
			"id", id, "backoff", backoff, "error", err)
	}
}
