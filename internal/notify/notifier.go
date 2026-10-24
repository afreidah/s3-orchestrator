// -------------------------------------------------------------------------------
// Notifier - Async Webhook Event Delivery
//
// Author: Alex Freidah
//
// Delivers CloudEvents JSON payloads to configured webhook endpoints via HTTP
// POST. Events are persisted in a notification_outbox table for durable retry
// with exponential backoff. The outbox is drained by a tick-runner service
// under its own advisory lock for multi-instance safety. Threshold-crossing
// events (capacity warnings) are dampened to avoid repeated notifications.
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
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/backoff"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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

// retryBackoff is how long a failed delivery waits before its next attempt: a
// second doubled per attempt, capped at a minute and four seconds.
var retryBackoff = backoff.Exponential{Base: time.Second, Ceiling: 64 * time.Second}

// Notifier delivers webhook notifications from a durable outbox queue. The
// embedded tick-runner service drains it and reports its health; Run wraps
// that service's Run to release what the notifier holds on shutdown.
type Notifier struct {
	*tickrunner.Service
	telemetry.SharedGauges

	log       *slog.Logger
	endpoints []config.NotificationEndpoint
	store     OutboxStore
	client    *http.Client
	dampener  *syncutil.TTLCache[string, struct{}]
}

// NewNotifier creates a notifier backed by the given outbox store. Registers
// itself as the process-wide event emitter so every package can publish
// notifications through the same mechanism. Endpoints carry the defaults
// config validation fills in.
func NewNotifier(cfg *config.NotificationConfig, store OutboxStore) *Notifier {
	const slug = "notifier"
	log := tickrunner.ComponentLogger(slug)
	n := &Notifier{
		log:       log,
		endpoints: cfg.Endpoints,
		store:     store,
		client: &http.Client{
			Timeout: httpClientTimeout,
		},
		dampener: syncutil.NewTTLCache[string, struct{}](dampenTTL),
	}
	n.Service = tickrunner.New(tickrunner.Config{
		Locker:   store,
		Interval: drainInterval,
		LockID:   core.LockNotifier,
		Name:     slug,
		Log:      log,
		Work:     tickrunner.QueueWork(log, "outbox drained", n.drain),
	})
	event.SetEmitter(n.emit)
	return n
}

// -------------------------------------------------------------------------
// EVENT EMISSION
// -------------------------------------------------------------------------

// emit persists an event to the outbox for each matching endpoint. Called
// through event.Publish from any package in the codebase.
func (n *Notifier) emit(ev event.Event) { //nolint:gocritic // Event is passed by value to allow callers to construct inline
	fillCloudEventDefaults(&ev)
	if n.isDampened(&ev) {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		n.log.ErrorContext(context.Background(), "notification event marshal failed", "type", ev.Type, "error", err)
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
		if ep.Prefix != "" && ev.Subject != "" && !strings.HasPrefix(ev.Subject, ep.Prefix) {
			continue
		}
		if err := n.store.InsertNotification(ctx, ev.Type, string(payload), ep.URL); err != nil {
			n.log.ErrorContext(ctx, "failed to enqueue notification",
				slog.String("type", ev.Type), slog.String("endpoint", ep.URL), "error", err)
			telemetry.NotificationDroppedTotal.Inc()
		}
	}
}

// -------------------------------------------------------------------------
// BACKGROUND DELIVERY WORKER
// -------------------------------------------------------------------------

// Run implements lifecycle.Runner. The embedded service drains the outbox on
// each tick until ctx ends, and the notifier is closed after it returns.
func (n *Notifier) Run(ctx context.Context) error {
	defer n.Close()
	return n.Service.Run(ctx)
}

// Close releases what the notifier holds beyond its own stack: the dampener's
// eviction goroutine. Run defers it, so a notifier the lifecycle manager stops
// needs nothing further; a caller that builds one without running it calls this
// itself, or leaves a goroutine behind per notifier.
func (n *Notifier) Close() {
	// Deregistered first: an event published after this point has no notifier
	// to deliver it, and leaving the hook installed would hand it to one whose
	// dampener has already stopped.
	event.SetEmitter(nil)
	if n.dampener != nil {
		n.dampener.Close()
	}
}

// drain delivers one batch of pending notifications, one at a time, and
// shares how many are still to be delivered: the whole outbox backlog, not
// the batch just processed. A failed count leaves the gauge as it was.
func (n *Notifier) drain(ctx context.Context) (succeeded, failed int) {
	rows, err := n.store.GetPendingNotifications(ctx, pendingBatchSize)
	if err != nil {
		n.log.ErrorContext(ctx, "failed to fetch pending notifications", "error", err)
		return 0, 0
	}
	runner := batch.Runner[core.NotificationRow]{Name: "notifier", Log: n.log, Concurrency: 1}
	sum := runner.Run(ctx, rows, n.processPendingRow)

	if depth, err := n.store.NotificationQueueDepth(ctx); err != nil {
		n.log.WarnContext(ctx, "failed to count pending notifications", "error", err)
	} else {
		n.PublishGauges(ctx, telemetry.GaugeSourceNotifier, telemetry.WorkerGauges{NotificationQueueDepth: &depth})
	}
	return sum.Succeeded, sum.Failed
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

	n.client.Timeout = ep.Timeout

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

// generateEventID creates a random hex event identifier.
func generateEventID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

// processPendingRow handles one outbox row: either the endpoint no longer
// exists (complete immediately), delivery failed (retry or exhaust), or
// delivery succeeded (complete + metrics).
func (n *Notifier) processPendingRow(ctx context.Context, row core.NotificationRow) batch.ItemResult {
	ep := n.findEndpoint(row.EndpointURL)
	if ep == nil {
		n.completeOrLog(ctx, row.ID, "no_matching_endpoint")
		return batch.ItemResult{Outcome: batch.ItemSkipped}
	}
	start := time.Now()
	if err := n.deliver(ctx, row, ep); err != nil {
		n.handleDeliveryFailure(ctx, row, ep, err)
		return batch.Failed()
	}
	telemetry.NotificationSentTotal.WithLabelValues(ep.URL, row.EventType).Inc()
	telemetry.NotificationDuration.WithLabelValues(ep.URL).Observe(time.Since(start).Seconds())
	n.completeOrLog(ctx, row.ID, "delivered")
	return batch.Succeeded()
}

// handleDeliveryFailure retries the row while the endpoint's max_retries
// allow, and otherwise drops it and counts the drop.
func (n *Notifier) handleDeliveryFailure(ctx context.Context, row core.NotificationRow, ep *config.NotificationEndpoint, err error) {
	telemetry.NotificationFailedTotal.WithLabelValues(ep.URL, row.EventType).Inc()
	if int(row.Attempts)+1 >= ep.MaxRetries {
		n.log.ErrorContext(ctx, "notification delivery exhausted",
			slog.String("endpoint", ep.URL),
			slog.String("event", row.EventType),
			slog.Int("attempts", int(row.Attempts+1)),
			"error", err)
		telemetry.NotificationDroppedTotal.Inc()
		n.completeOrLog(ctx, row.ID, "exhausted")
		return
	}
	n.retryOrLog(ctx, row.ID, retryBackoff.For(row.Attempts), err.Error())
}

// completeOrLog marks a notification row complete in the outbox. Store
// errors are logged at WARN and counted in the notification-store-errors
// metric rather than silently discarded, since a dropped Complete can lead
// to a webhook being delivered twice on the next drain tick.
func (n *Notifier) completeOrLog(ctx context.Context, id int64, reason string) {
	if err := n.store.CompleteNotification(ctx, id); err != nil {
		telemetry.NotificationStoreErrorsTotal.WithLabelValues("complete").Inc()
		n.log.WarnContext(ctx, "completeNotification failed",
			slog.Int64("notification_id", id), slog.String("reason", reason), "error", err)
	}
}

// retryOrLog schedules a notification for retry. Store errors are logged
// and counted (see completeOrLog) rather than silently discarded.
func (n *Notifier) retryOrLog(ctx context.Context, id int64, backoff time.Duration, reason string) {
	if err := n.store.RetryNotification(ctx, id, backoff, reason); err != nil {
		telemetry.NotificationStoreErrorsTotal.WithLabelValues("retry").Inc()
		n.log.WarnContext(ctx, "retryNotification failed",
			slog.Int64("notification_id", id), slog.Duration("backoff", backoff), "error", err)
	}
}
