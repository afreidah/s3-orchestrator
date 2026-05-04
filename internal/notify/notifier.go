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

// Package notify drains the notification_outbox table and delivers
// each pending event to its configured webhook target with retries
// and exponential backoff under an advisory lock for multi-instance
// safety.
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
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// OUTBOX STORE INTERFACE
// -------------------------------------------------------------------------

// OutboxStore defines the persistence methods the notifier needs for
// durable event delivery. Composed of the durable outbox row operations
// plus the advisory-lock helper used to serialize the drain loop across
// replicas. Both core.NotificationOutbox and core.AdvisoryLocker are
// satisfied by the engine concrete *Store.
type OutboxStore interface {
	core.NotificationOutbox
	core.AdvisoryLocker
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
	fillCloudEventDefaults(&ev)
	if n.isDampened(&ev) {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		slog.Error("Failed to marshal notification event", "type", ev.Type, "error", err) //nolint:sloglint // emit has no request context
		return
	}
	n.fanOutToEndpoints(&ev, payload)
}

// fillCloudEventDefaults populates CloudEvents envelope fields that the
// caller didn't set. Mutates the event in place.
func fillCloudEventDefaults(ev *event.Event) {
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
}

// isDampened returns true when the event is a threshold-crossing type
// already emitted recently. The dampener prevents repeated capacity
// warnings from flooding downstream pipelines.
func (n *Notifier) isDampened(ev *event.Event) bool {
	if ev.Type != event.BackendCapacityWarning || n.dampener == nil {
		return false
	}
	dampenKey := ev.Type + ":" + ev.Subject
	if _, ok := n.dampener.Get(dampenKey); ok {
		telemetry.NotificationDroppedTotal.Inc()
		return true
	}
	n.dampener.Set(dampenKey, struct{}{})
	return false
}

// fanOutToEndpoints inserts one outbox row per configured endpoint whose
// filter + prefix matches the event. The delivery worker picks the rows
// up on its next drain tick.
func (n *Notifier) fanOutToEndpoints(ev *event.Event, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()

	for _, ep := range n.endpoints {
		if !event.MatchesFilter(ev.Type, ep.Events) {
			continue
		}
		if ep.Prefix != "" && ev.Subject != "" && !hasPrefix(ev.Subject, ep.Prefix) {
			continue
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
			n.processPendingRow(lockCtx, row)
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
func (n *Notifier) deliver(ctx context.Context, row core.NotificationRow, ep *config.NotificationEndpoint) error {
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

// processPendingRow handles one outbox row: either the endpoint no longer
// exists (complete immediately), delivery failed (retry or exhaust), or
// delivery succeeded (complete + metrics).
func (n *Notifier) processPendingRow(ctx context.Context, row core.NotificationRow) {
	ep := n.findEndpoint(row.EndpointURL)
	if ep == nil {
		n.completeOrLog(ctx, row.ID, "no_matching_endpoint")
		return
	}
	start := time.Now()
	err := n.deliver(ctx, row, ep)
	if err != nil {
		n.handleDeliveryFailure(ctx, row, ep, err)
		return
	}
	telemetry.NotificationSentTotal.WithLabelValues(ep.URL, row.EventType).Inc()
	telemetry.NotificationDuration.WithLabelValues(ep.URL).Observe(time.Since(start).Seconds())
	n.completeOrLog(ctx, row.ID, "delivered")
}

// handleDeliveryFailure decides whether to schedule a retry or give up,
// based on the endpoint's configured max retries and the row's current
// attempt count.
func (n *Notifier) handleDeliveryFailure(ctx context.Context, row core.NotificationRow, ep *config.NotificationEndpoint, err error) {
	telemetry.NotificationFailedTotal.WithLabelValues(ep.URL, row.EventType).Inc()
	backoff := time.Duration(1<<min(row.Attempts, maxBackoffShift)) * time.Second
	maxRetries := ep.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	if int(row.Attempts)+1 >= maxRetries {
		slog.ErrorContext(ctx, "notification delivery exhausted",
			"endpoint", ep.URL, "event", row.EventType, "attempts", row.Attempts+1, "error", err)
		n.completeOrLog(ctx, row.ID, "exhausted")
		return
	}
	n.retryOrLog(ctx, row.ID, backoff, err.Error())
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
