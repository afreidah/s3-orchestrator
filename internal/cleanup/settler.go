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

// Store is the cleanup queue surface the settler writes outcomes to.
type Store interface {
	CompleteCleanupItem(ctx context.Context, id int64) error
	RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error)
}

// Deps groups the settler's constructor parameters. Concurrency bounds both the
// backends deleted from at once and the rows settled at once. Admit, when set,
// wraps each backend's delete so a background caller can take an admission
// slot; a backend it declines is left unsettled for a later attempt. Name
// labels the cycle log line; an empty Name settles without one.
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

// maxAttempts is the retry ceiling. The 1-minute starting backoff doubled 10
// times yields ~17 hours of total retry runway, which is enough to bridge the
// longest realistic backend outages. Beyond that the row graduates to
// cleanup_dlq for operator action.
const maxAttempts = 10

// logMsgCompleteFailed is the error log message emitted when retiring a row
// fails, shared by every path that retires one.
const logMsgCompleteFailed = "failed to complete cleanup item"

// The statuses a settled row reports, which label CleanupQueueProcessedTotal.
const (
	statusSuccess   = "success"
	statusAbsent    = "success_absent"
	statusRetry     = "retry"
	statusExhausted = "exhausted"
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
// The outcomes are written on a context the caller's cancellation does not
// reach, so a client that hangs up after its transaction committed still
// leaves each row settled or retried rather than claimed until the grace
// period passes.
func (s *Settler) Settle(ctx context.Context, items []core.CleanupItem) batch.Summary {
	if len(items) == 0 {
		return batch.Summary{}
	}
	deleted := s.deleteAll(ctx, items)
	settleCtx := context.WithoutCancel(ctx)
	var runLog *slog.Logger
	if s.name != "" {
		runLog = s.log
	}
	runner := batch.Runner[core.CleanupItem]{Name: s.name, Log: runLog, Concurrency: s.concurrency}
	return runner.Run(settleCtx, items, func(ctx context.Context, item core.CleanupItem) batch.ItemResult {
		d, attempted := deleted[item.ID]
		if !attempted {
			return batch.ItemResult{} // admission declined its backend; the row waits for a later attempt
		}
		return s.settle(ctx, &item, d)
	})
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

// settle gives one row its outcome from deleting its bytes: retired, retried,
// or graduated to the DLQ. A 404 means the backend already agrees the bytes
// are gone, and a backend no longer registered cannot hold an orphan the fleet
// knows about, so both retire the row.
func (s *Settler) settle(ctx context.Context, item *core.CleanupItem, d deletion) batch.ItemResult {
	switch {
	case d.unknownBackend:
		s.log.WarnContext(ctx, "backend not found, removing item",
			"backend", item.BackendName, "key", item.ObjectKey)
		s.complete(ctx, item, statusSuccess, "")
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: statusSuccess}
	case d.err == nil:
		s.complete(ctx, item, statusSuccess, "cleanup_queue.processed")
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: statusSuccess}
	case backend.IsNotFound(d.err):
		s.complete(ctx, item, statusAbsent, "cleanup_queue.already_absent")
		return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: statusAbsent}
	}

	attempts := item.Attempts + 1
	if attempts >= maxAttempts {
		s.exhaust(ctx, item, attempts, d.err)
		return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusExhausted}
	}
	s.retry(ctx, item, d.err)
	return batch.ItemResult{Outcome: batch.ItemFailed, Status: statusRetry}
}

// complete retires a row whose bytes are gone, counts it under status, and
// records auditEvent when one is named.
func (s *Settler) complete(ctx context.Context, item *core.CleanupItem, status, auditEvent string) {
	if err := s.store.CompleteCleanupItem(ctx, item.ID); err != nil {
		s.log.ErrorContext(ctx, logMsgCompleteFailed, slog.Int64("cleanup_id", item.ID), "error", err)
	}
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(status).Inc()
	if auditEvent == "" {
		return
	}
	audit.Log(ctx, auditEvent,
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.String("reason", item.Reason),
		slog.Int("attempt", int(item.Attempts+1)),
	)
}

// exhaust moves a row whose retries are spent into the dead-letter queue, with
// an audit entry and a CleanupExhausted event so operators can investigate.
func (s *Settler) exhaust(ctx context.Context, item *core.CleanupItem, attempts int32, delErr error) {
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(statusExhausted).Inc()
	s.log.ErrorContext(ctx, "max attempts reached, moving to DLQ",
		slog.String("key", item.ObjectKey),
		slog.String("backend", item.BackendName),
		slog.Int("attempts", int(attempts)),
		slog.Int64("size_bytes", item.SizeBytes),
		"error", delErr)
	moved, err := s.store.MoveCleanupToDLQ(ctx, item.ID, delErr.Error())
	if err != nil {
		s.log.ErrorContext(ctx, "failed to move cleanup item to DLQ",
			slog.Int64("cleanup_id", item.ID), "error", err)
		return
	}
	if !moved {
		return
	}
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
}

// retry stamps the next retry deadline on a row still within its budget and
// releases its claim.
func (s *Settler) retry(ctx context.Context, item *core.CleanupItem, delErr error) {
	telemetry.CleanupQueueProcessedTotal.WithLabelValues(statusRetry).Inc()
	if err := s.store.RetryCleanupItem(ctx, item.ID, Backoff(item.Attempts), delErr.Error()); err != nil {
		s.log.ErrorContext(ctx, "failed to update cleanup retry", "id", item.ID, "error", err)
	}
}
