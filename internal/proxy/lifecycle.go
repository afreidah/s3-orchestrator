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

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// ProcessLifecycleRules evaluates all lifecycle rules and deletes expired
// objects. Returns total deleted and failed counts. Terminates processing
// of a rule when a full batch produces zero successful deletions, preventing
// infinite loops when backends are unhealthy.
func (m *BackendManager) ProcessLifecycleRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int) {
	cfg := m.LifecycleConfig()
	batchSize := 100
	if cfg != nil && cfg.BatchSize > 0 {
		batchSize = cfg.BatchSize
	}

	ctx, span := telemetry.StartSpan(ctx, "ProcessLifecycleRules",
		telemetry.AttrOperation.String("lifecycle"),
	)
	defer span.End()

	for _, rule := range rules {
		cutoff := time.Now().Add(-time.Duration(rule.ExpirationDays) * 24 * time.Hour)

		for {
			objects, err := m.store.ListExpiredObjects(ctx, rule.Prefix, cutoff, batchSize)
			if err != nil {
				slog.ErrorContext(ctx, "Lifecycle: failed to list expired objects",
					"prefix", rule.Prefix, "error", err)
				failed++
				break
			}

			if len(objects) == 0 {
				break
			}

			batchDeleted := 0
			for i := range objects {
				if err := m.ObjectManager.DeleteObject(ctx, objects[i].ObjectKey); err != nil {
					slog.WarnContext(ctx, "Lifecycle: failed to delete expired object",
						"key", objects[i].ObjectKey, "error", err)
					telemetry.LifecycleFailedTotal.Inc()
					failed++
				} else {
					audit.Log(ctx, "lifecycle.delete",
						slog.String("key", objects[i].ObjectKey),
						slog.String("prefix", rule.Prefix),
						slog.Int("expiration_days", rule.ExpirationDays),
					)
					telemetry.LifecycleDeletedTotal.Inc()
					batchDeleted++
					deleted++
				}
			}

			// Stop if no progress was made to prevent infinite loops
			// when all backends are unhealthy.
			if batchDeleted == 0 {
				slog.WarnContext(ctx, "Lifecycle: batch yielded zero deletions, stopping rule",
					"prefix", rule.Prefix, "batch_failed", len(objects))
				break
			}

			// Exhausted this rule when the batch is smaller than requested.
			if len(objects) < batchSize {
				break
			}
		}
	}
	return deleted, failed
}
