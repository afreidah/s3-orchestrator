// -------------------------------------------------------------------------------
// Lifecycle - Automatic Object Expiration
//
// Author: Alex Freidah
//
// Evaluates lifecycle rules and deletes objects whose created_at timestamp is
// older than the configured expiration period. Reuses the existing DeleteObject
// path for quota decrement, cache invalidation, and cleanup queue.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

const lifecycleBatchSize = 100

// ProcessLifecycleRules evaluates all lifecycle rules and deletes expired
// objects. Returns total deleted and failed counts.
func (m *BackendManager) ProcessLifecycleRules(ctx context.Context, rules []config.LifecycleRule) (deleted, failed int) {
	for _, rule := range rules {
		cutoff := time.Now().Add(-time.Duration(rule.ExpirationDays) * 24 * time.Hour)

		for {
			objects, err := m.store.ListExpiredObjects(ctx, rule.Prefix, cutoff, lifecycleBatchSize)
			if err != nil {
				slog.Error("Lifecycle: failed to list expired objects",
					"prefix", rule.Prefix, "error", err)
				failed++
				break
			}

			if len(objects) == 0 {
				break
			}

			for _, obj := range objects {
				if err := m.DeleteObject(ctx, obj.ObjectKey); err != nil {
					slog.Warn("Lifecycle: failed to delete expired object",
						"key", obj.ObjectKey, "error", err)
					telemetry.LifecycleFailedTotal.Inc()
					failed++
				} else {
					slog.Debug("Lifecycle: deleted expired object",
						"key", obj.ObjectKey, "prefix", rule.Prefix,
						"age_days", rule.ExpirationDays)
					telemetry.LifecycleDeletedTotal.Inc()
					deleted++
				}
			}

			// If we got fewer than batchSize, we've exhausted this rule.
			if len(objects) < lifecycleBatchSize {
				break
			}
		}
	}
	return deleted, failed
}
