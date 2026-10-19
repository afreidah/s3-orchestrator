// -------------------------------------------------------------------------------
// Cleanup Settler - Delete Queued Bytes and Settle Their Rows
//
// Author: Alex Freidah
//
// Every transaction that stops recording bytes queues their deletion as a
// cleanup_queue row before it commits. The settler is what turns those rows
// into deleted bytes: it deletes each backend's paths in one batched request,
// then gives every row its outcome - retired, retried with backoff, or
// graduated to the dead-letter queue. The request path settles the rows its own
// transaction queued as soon as it commits, and the cleanup worker settles the
// rows it claims on each tick, so both run the same code.
// -------------------------------------------------------------------------------

package cleanup

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Deleter is the backend surface the settler deletes through.
type Deleter interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
}

// Store is the cleanup queue surface the settler writes outcomes to. Each write
// matches the claim the row was read under and reports a lost claim, so an
// instance whose claim was taken over cannot settle the row again.
type Store interface {
	CompleteCleanupItems(ctx context.Context, items []core.CleanupItem) ([]int64, error)
	RetryCleanupItem(ctx context.Context, item *core.CleanupItem, backoff time.Duration, lastError string) (bool, error)
	MoveCleanupToDLQ(ctx context.Context, item *core.CleanupItem, lastError string) (bool, error)
}

// Deps groups the settler's constructor parameters. Concurrency bounds the
// backends deleted from at once. Admit, when set, wraps each backend's delete
// so a background caller can take an admission slot; a backend it declines is
// left unsettled for a later attempt. Name labels the cycle log line; an empty
// Name settles without one.
type Deps struct {
	Deleter     Deleter
	Store       Store
	Concurrency int
	Admit       func(ctx context.Context, fn func())
	Name        string
}

// Settler deletes queued bytes and settles their cleanup rows.
type Settler struct {
	deleter     Deleter
	store       Store
	concurrency int
	admit       func(ctx context.Context, fn func())
	name        string
	log         *slog.Logger
}

// New builds a Settler from its dependencies.
func New(d Deps) *Settler {
	must.NotNil("Deleter", d.Deleter)
	must.NotNil("Store", d.Store)
	return &Settler{
		deleter:     d.Deleter,
		store:       d.Store,
		concurrency: max(d.Concurrency, 1),
		admit:       d.Admit,
		name:        d.Name,
		log:         slog.Default().With(logfmt.Component("cleanup")),
	}
}

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The statuses a settled row reports, which label CleanupQueueProcessedTotal.
// statusClaimLost is a row another instance took over before this one could
// settle it; its new holder settles it.
const (
	statusSuccess   = "success"
	statusAbsent    = "success_absent"
	statusRetry     = "retry"
	statusExhausted = "exhausted"
	statusClaimLost = "claim_lost"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Backoff returns the backoff duration for the given attempt number:
// min(1m * 2^attempts, 24h). It short-circuits the shift for attempts >= 11
// (where the doubling already exceeds the cap) and for negative inputs, since
// shifting by a negative or out-of-range count is undefined in Go.
func Backoff(attempts int32) time.Duration {
	const maxBackoff = 24 * time.Hour
	if attempts < 0 || attempts >= 11 {
		return maxBackoff
	}
	return min(time.Minute<<attempts, maxBackoff)
}

// Settle deletes every row's bytes, one batched delete per backend, and gives
// each row its outcome. It deletes each row's queued path, not the object's
// key: the row names the bytes one write put on the backend, and a later write
// of the same object has its own bytes this must not reach.
//
// Every row whose bytes are gone is retired in one store call, so a settle
// costs one commit however many rows it retires; retries and dead-letter moves
// are written per row. The outcomes are written on a context the caller's
// cancellation does not reach, so a client that hangs up after its transaction
// committed still leaves each row settled or retried rather than claimed until
// the grace period passes.
func (s *Settler) Settle(ctx context.Context, items []core.CleanupItem) batch.Summary {
	if len(items) == 0 {
		return batch.Summary{}
	}
	start := time.Now()
	deleted := s.deleteAll(ctx, items)
	ctx = context.WithoutCancel(ctx)

	// A row whose backend admission declined keeps the zero result, a skip,
	// and waits for a later attempt.
	results := make([]batch.ItemResult, len(items))
	var gone []int
	for i := range items {
		d, attempted := deleted[items[i].ID]
		switch {
		case !attempted:
		case d.gone():
			gone = append(gone, i)
		default:
			results[i] = s.fail(ctx, &items[i], d.err)
		}
	}
	s.retire(ctx, items, gone, deleted, results)

	sum := batch.Tally(results, start)
	if s.name != "" {
		batch.LogCycle(ctx, s.log, s.name, sum)
	}
	return sum
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// deletion is what deleting one row's bytes came to. unknownBackend means the
// row names a backend that is no longer registered, so nothing was attempted.
type deletion struct {
	err            error
	unknownBackend bool
}

// gone reports whether the row's bytes need no further delete: the delete
// landed, the backend already agrees they are absent, or the backend is no
// longer registered and so cannot hold an orphan the fleet knows about.
func (d deletion) gone() bool {
	return d.unknownBackend || d.err == nil || backend.IsNotFound(d.err)
}

// outcome is how a gone row is reported once retired: its status and the
// audit event that records it, none for a backend no longer registered.
func (d deletion) outcome() (status, auditEvent string) {
	switch {
	case d.unknownBackend:
		return statusSuccess, ""
	case d.err == nil:
		return statusSuccess, "cleanup_queue.processed"
	default:
		return statusAbsent, "cleanup_queue.already_absent"
	}
}

// deleteAll deletes every row's bytes, one batched delete per backend, each
// wrapped in the admission hook when one is set.
func (s *Settler) deleteAll(ctx context.Context, items []core.CleanupItem) map[int64]deletion {
	byBackend := make(map[string][]core.CleanupItem)
	for i := range items {
		byBackend[items[i].BackendName] = append(byBackend[items[i].BackendName], items[i])
	}

	var mu sync.Mutex
	deleted := make(map[int64]deletion, len(items))
	record := func(id int64, d deletion) {
		mu.Lock()
		deleted[id] = d
		mu.Unlock()
	}
	groups := slices.Collect(maps.Values(byBackend))
	workerpool.Run(ctx, s.concurrency, groups, func(ctx context.Context, group []core.CleanupItem) {
		name := group[0].BackendName
		be, err := s.deleter.GetBackend(name)
		if err != nil {
			for i := range group {
				record(group[i].ID, deletion{unknownBackend: true})
			}
			return
		}
		paths := make([]string, len(group))
		for i := range group {
			paths[i] = core.StoragePath(group[i].ObjectKey, group[i].StorageKey)
		}
		run := func() {
			failed := s.deleter.DeleteMany(ctx, name, be, paths)
			for i := range group {
				record(group[i].ID, deletion{err: failed[paths[i]]})
			}
		}
		if s.admit == nil {
			run()
			return
		}
		s.admit(ctx, run)
	})
	return deleted
}

// retire retires every gone row in one store call and reports each: retired
// rows under their deletion's status and audit event, rows whose claim was
// taken over as lost. A failed call leaves the rows claimed, so the cleanup
// worker takes them over after the grace period and finds the bytes absent.
func (s *Settler) retire(ctx context.Context, items []core.CleanupItem, gone []int, deleted map[int64]deletion, results []batch.ItemResult) {
	if len(gone) == 0 {
		return
	}
	retiring := make([]core.CleanupItem, len(gone))
	for j, i := range gone {
		retiring[j] = items[i]
	}
	retired, err := s.store.CompleteCleanupItems(ctx, retiring)
	if err != nil {
		s.log.ErrorContext(ctx, "failed to complete cleanup items", slog.Int("rows", len(retiring)), "error", err)
		return
	}
	for _, i := range gone {
		item := &items[i]
		if !slices.Contains(retired, item.ID) {
			results[i] = s.lost(ctx, item)
			continue
		}
		d := deleted[item.ID]
		if d.unknownBackend {
			s.log.WarnContext(ctx, "backend not found, removing item",
				"backend", item.BackendName, "key", item.ObjectKey)
		}
		status, auditEvent := d.outcome()
		results[i] = s.retired(ctx, item, status, auditEvent)
	}
}

// retired counts a row retired under status and records auditEvent when one
// is named.
func (s *Settler) retired(ctx context.Context, item *core.CleanupItem, status, auditEvent string) batch.ItemResult {
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(status).Inc()
	if auditEvent != "" {
		audit.Log(ctx, auditEvent,
			slog.String("key", item.ObjectKey),
			slog.String("backend", item.BackendName),
			slog.String("reason", item.Reason),
			slog.Int("attempt", int(item.Attempts+1)),
		)
	}
	return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: status}
}

// fail settles a row whose delete failed: retried while its budget lasts,
// graduated to the DLQ once it is spent.
func (s *Settler) fail(ctx context.Context, item *core.CleanupItem, delErr error) batch.ItemResult {
	attempts := item.Attempts + 1
	if attempts >= core.MaxCleanupAttempts {
		return s.exhaust(ctx, item, attempts, delErr)
	}
	return s.retry(ctx, item, delErr)
}

// lost reports a row whose claim another instance took over before this one
// settled it. The new holder settles it, so this one leaves it alone.
func (s *Settler) lost(ctx context.Context, item *core.CleanupItem) batch.ItemResult {
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(statusClaimLost).Inc()
	s.log.InfoContext(ctx, "cleanup row claimed by another instance, leaving it",
		slog.Int64("cleanup_id", item.ID), slog.String("backend", item.BackendName))
	return batch.ItemResult{Outcome: batch.ItemSkipped, Status: statusClaimLost}
}

// exhaust moves a row whose retries are spent into the dead-letter queue, with
// an audit entry and a CleanupExhausted event so operators can investigate.
func (s *Settler) exhaust(ctx context.Context, item *core.CleanupItem, attempts int32, delErr error) batch.ItemResult {
	s.log.ErrorContext(ctx, "max attempts reached, moving to DLQ",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.Int("attempts", int(attempts)),
		slog.Int64("size_bytes", item.SizeBytes),
		"error", delErr)
	moved, err := s.store.MoveCleanupToDLQ(ctx, item, delErr.Error())
	if err != nil {
		s.log.ErrorContext(ctx, "failed to move cleanup item to DLQ",
			slog.Int64("cleanup_id", item.ID), "error", err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusExhausted}
	}
	if !moved {
		return s.lost(ctx, item)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(statusExhausted).Inc()
	telemetry.CleanupDLQEnqueuedTotal.WithLabelValues(item.BackendName).Inc()
	audit.Log(ctx, "cleanup_queue.exhausted_to_dlq",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.String("reason", item.Reason),
		slog.Int("attempts", int(attempts)),
		slog.Int64("size_bytes", item.SizeBytes),
		slog.String("last_error", delErr.Error()),
	)
	event.Publish(event.CleanupExhausted, item.BackendName, map[string]any{
		"backend":    item.BackendName,
		"object_key": item.ObjectKey,
		"reason":     item.Reason,
		"attempts":   int(attempts),
		"size_bytes": item.SizeBytes,
		"last_error": delErr.Error(),
	})
	return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusExhausted}
}

// retry stamps the next retry deadline on a row still within its budget and
// releases its claim.
func (s *Settler) retry(ctx context.Context, item *core.CleanupItem, delErr error) batch.ItemResult {
	retried, err := s.store.RetryCleanupItem(ctx, item, Backoff(item.Attempts), delErr.Error())
	if err != nil {
		s.log.ErrorContext(ctx, "failed to update cleanup retry", "id", item.ID, "error", err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusRetry}
	}
	if !retried {
		return s.lost(ctx, item)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(statusRetry).Inc()
	return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusRetry}
}
