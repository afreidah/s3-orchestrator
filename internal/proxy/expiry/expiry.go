// -------------------------------------------------------------------------------
// Expiry - Lifecycle Rule Evaluation
//
// Author: Alex Freidah
//
// Evaluates lifecycle rules and deletes objects whose created_at is older than
// the configured expiration period. Deletion goes through the normal object
// delete path so quota decrement, cache invalidation, and the cleanup queue all
// behave exactly as they do for a client-issued delete.
//
// Owns the reloadable lifecycle config rather than reading it from a facade:
// the rules and the code that applies them belong together, and the reload
// hook writes here directly.
// -------------------------------------------------------------------------------

package expiry

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

//go:generate mockgen -destination=mock_test.go -package=expiry github.com/afreidah/s3-orchestrator/internal/proxy/expiry ObjectDeleter

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// defaultBatchSize bounds one store query when the operator configured none.
const defaultBatchSize = 100

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ObjectDeleter removes one object through the full delete path.
// *object.Manager satisfies it.
type ObjectDeleter interface {
	DeleteObject(ctx context.Context, key string) error
}

// Manager applies lifecycle rules on demand. Safe for concurrent use; the
// config is swapped atomically by the reload path.
type Manager struct {
	store   core.ExpiredObjectsLister
	objects ObjectDeleter
	cfg     syncutil.AtomicConfig[config.LifecycleConfig]
	log     *slog.Logger
}

// New builds a Manager. log may be nil, in which case the default logger is
// used at call time.
func New(store core.ExpiredObjectsLister, objects ObjectDeleter, log *slog.Logger) *Manager {
	return &Manager{store: store, objects: objects, log: log}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// SetConfig atomically replaces the lifecycle configuration.
func (m *Manager) SetConfig(cfg *config.LifecycleConfig) { m.cfg.Store(cfg) }

// Config returns the current lifecycle configuration, or nil when unset.
func (m *Manager) Config() *config.LifecycleConfig { return m.cfg.Load() }

// logger returns the configured logger, falling back to the default so a
// Manager built without one in a test still logs through the standard handler.
func (m *Manager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// ProcessRules evaluates every rule and deletes the objects each one expires,
// returning the tally across all rules: Succeeded counts the objects deleted.
// obs receives one bracketed step per object and is nil on the scheduled tick.
func (m *Manager) ProcessRules(ctx context.Context, rules []config.LifecycleRule, obs progress.Observer) batch.Summary {
	batchSize := batchSizeFor(m.Config())

	ctx, span := telemetry.StartSpan(ctx, "ProcessLifecycleRules",
		telemetry.AttrOperation.String("lifecycle"),
	)
	defer span.End()

	var total batch.Summary
	for _, rule := range rules {
		total = total.Plus(m.applyRule(ctx, rule, batchSize, obs))
	}
	return total
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// batchSizeFor returns the per-tick batch size, falling back to the default
// when the operator configured none.
func batchSizeFor(cfg *config.LifecycleConfig) int {
	if cfg != nil && cfg.BatchSize > 0 {
		return cfg.BatchSize
	}
	return defaultBatchSize
}

// applyRule walks one rule's expired objects by key and deletes each. An object
// whose delete fails is passed over, so a backend outage costs each object one
// attempt per sweep rather than one per page. A listing failure counts as one
// failure and ends the rule.
func (m *Manager) applyRule(ctx context.Context, rule config.LifecycleRule, batchSize int, obs progress.Observer) batch.Summary {
	cutoff := time.Now().Add(-time.Duration(rule.ExpirationDays) * 24 * time.Hour)
	pager := batch.Pager[core.ObjectLocation, string]{
		PageSize: batch.FixedPage(batchSize),
		List: func(ctx context.Context, limit int, after string) ([]core.ObjectLocation, error) {
			return m.store.ListExpiredObjects(ctx, core.ExpiredObjectsQuery{
				Prefix: rule.Prefix,
				Tags:   rule.Tags,
				Cutoff: cutoff,
				After:  after,
				Limit:  limit,
			})
		},
		CursorOf: func(o core.ObjectLocation) string { return o.ObjectKey },
	}
	runner := batch.Runner[core.ObjectLocation]{
		Name:        "lifecycle",
		Concurrency: 1,
		Observer:    obs,
		Key:         func(o core.ObjectLocation) string { return o.ObjectKey },
	}
	var total batch.Summary
	stop, err := pager.Walk(ctx, func(ctx context.Context, objects []core.ObjectLocation) (batch.Step, error) {
		sum := runner.Run(ctx, objects, func(ctx context.Context, o core.ObjectLocation) batch.ItemResult {
			if err := m.deleteExpired(ctx, rule, o.ObjectKey); err != nil {
				return batch.ItemResult{Outcome: batch.ItemFailed, Status: progress.StatusFailed}
			}
			return batch.ItemResult{Outcome: batch.ItemSucceeded, Status: progress.StatusOK}
		})
		total = total.Plus(sum)
		return batch.Step{Progress: sum.Succeeded}, nil
	})
	if stop == batch.Errored {
		m.logger().ErrorContext(ctx, "failed to list expired objects",
			slog.String("prefix", rule.Prefix), "error", err)
		total.Failed++
	}
	return total
}

// deleteExpired removes one expired object and records the outcome, so the
// runner's per-item bracketing reports a status rather than carrying the work.
func (m *Manager) deleteExpired(ctx context.Context, rule config.LifecycleRule, key string) error {
	if err := m.objects.DeleteObject(ctx, key); err != nil {
		m.logger().WarnContext(ctx, "failed to delete expired object",
			slog.String("key", key), "error", err)
		telemetry.LifecycleFailedTotal.Inc()
		return err
	}
	audit.Log(ctx, "lifecycle.delete",
		slog.String("key", key),
		slog.String("prefix", rule.Prefix),
		slog.Int("expiration_days", rule.ExpirationDays),
	)
	bucket, userKey := internalkey.Split(key)
	event.Publish(event.LifecycleDelete, userKey, map[string]any{
		"bucket":          bucket,
		"key":             userKey,
		"prefix":          rule.Prefix,
		"expiration_days": rule.ExpirationDays,
	})
	telemetry.LifecycleDeletedTotal.Inc()
	return nil
}
