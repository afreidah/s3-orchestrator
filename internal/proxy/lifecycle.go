// -------------------------------------------------------------------------------
// Lifecycle - Automatic Object Expiration
//
// Author: Alex Freidah
//
// Evaluates lifecycle rules and deletes objects whose created_at timestamp is
// older than the configured expiration period. Reuses the existing DeleteObject
// path for quota decrement, cache invalidation, and cleanup queue. Terminates
// early when a full batch yields zero successful deletions to prevent infinite
// loops during backend outages.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ProcessLifecycleRules evaluates all lifecycle rules and deletes expired
// objects. Returns total deleted and failed counts. Terminates processing
// of a rule when a full batch produces zero successful deletions, preventing
// infinite loops when backends are unhealthy.
func (m *BackendManager) ProcessLifecycleRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int) {
	batchSize := lifecycleBatchSize(m.LifecycleConfig())

	ctx, span := telemetry.StartSpan(ctx, "ProcessLifecycleRules",
		telemetry.AttrOperation.String("lifecycle"),
	)
	defer span.End()

	for _, rule := range rules {
		d, f := m.applyLifecycleRule(ctx, rule, batchSize)
		deleted += d
		failed += f
	}
	return deleted, failed
}

// lifecycleBatchSize returns the batch size to use per lifecycle tick.
// Falls back to 100 when the caller hasn't set one.
func lifecycleBatchSize(cfg *config.LifecycleConfig) int {
	if cfg != nil && cfg.BatchSize > 0 {
		return cfg.BatchSize
	}
	return 100
}

// applyLifecycleRule runs a single rule to completion (or until the store
// stops returning new expired objects, or a full batch produces zero
// successful deletions  -  the infinite-loop guard).
func (m *BackendManager) applyLifecycleRule(ctx context.Context, rule config.LifecycleRule, batchSize int) (deleted, failed int) {
	cutoff := time.Now().Add(-time.Duration(rule.ExpirationDays) * 24 * time.Hour)
	for {
		objects, err := m.stores.ListExpiredObjects(ctx, rule.Prefix, cutoff, batchSize)
		if err != nil {
			slog.ErrorContext(ctx, "failed to list expired objects",
				slog.String("prefix", rule.Prefix), "error", err)
			failed++
			return deleted, failed
		}
		if len(objects) == 0 {
			return deleted, failed
		}
		batchDeleted, batchFailed := m.deleteLifecycleBatch(ctx, rule, objects)
		deleted += batchDeleted
		failed += batchFailed

		if batchDeleted == 0 {
			slog.WarnContext(ctx, "batch yielded zero deletions, stopping rule",
				"prefix", rule.Prefix, "batch_failed", len(objects))
			return deleted, failed
		}
		if len(objects) < batchSize {
			return deleted, failed
		}
	}
}

// deleteLifecycleBatch processes one batch of expired objects, emitting
// audit + metrics for each outcome. Returns (deleted, failed) counts.
func (m *BackendManager) deleteLifecycleBatch(ctx context.Context, rule config.LifecycleRule, objects []core.ObjectLocation) (deleted, failed int) {
	for i := range objects {
		if err := m.ObjectManager.DeleteObject(ctx, objects[i].ObjectKey); err != nil {
			slog.WarnContext(ctx, "failed to delete expired object",
				slog.String("key", objects[i].ObjectKey), "error", err)
			telemetry.LifecycleFailedTotal.Inc()
			failed++
			continue
		}
		audit.Log(ctx, "lifecycle.delete",
			slog.String("key", objects[i].ObjectKey),
			slog.String("prefix", rule.Prefix),
			slog.Int("expiration_days", rule.ExpirationDays),
		)
		telemetry.LifecycleDeletedTotal.Inc()
		deleted++
	}
	return deleted, failed
}
