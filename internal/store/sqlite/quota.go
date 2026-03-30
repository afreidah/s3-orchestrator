// -------------------------------------------------------------------------------
// SQLite Quota and Usage - Backend Space Management and Usage Tracking
//
// Author: Alex Freidah
//
// Implements quota enforcement, backend space selection, usage delta flushing,
// and orphan byte tracking for the SQLite backend. Uses dynamic IN clause
// expansion for backend-filtered queries.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// GetBackendWithSpace finds a backend with enough quota for the given size.
// Returns the backend name or ErrNoSpaceAvailable if none have enough space.
func (s *Store) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	for _, name := range backendOrder {
		var available int64
		err := s.db.QueryRowContext(ctx, `
			SELECT CASE
				WHEN q.bytes_limit = 0 THEN 9223372036854775807
				ELSE (q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(m.inflight, 0))
			END AS available
			FROM backend_quotas q
			LEFT JOIN (
				SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
				FROM multipart_uploads mu
				JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
				GROUP BY mu.backend_name
			) m ON m.backend_name = q.backend_name
			WHERE q.backend_name = ?`, name).Scan(&available)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to check quota for %s: %w", name, err)
		}

		if available >= size {
			return name, nil
		}
	}

	return "", store.ErrNoSpaceAvailable
}

// GetLeastUtilizedBackend finds the backend with the lowest utilization ratio
// that has enough space for the given size. Used by the "spread" routing strategy.
func (s *Store) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	if len(eligible) == 0 {
		return "", store.ErrNoSpaceAvailable
	}

	query := `
		SELECT q.backend_name
		FROM backend_quotas q
		LEFT JOIN (
			SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
			FROM multipart_uploads mu
			JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
			GROUP BY mu.backend_name
		) m ON m.backend_name = q.backend_name
		WHERE q.backend_name IN (` + placeholders(len(eligible)) + `)
		  AND CASE WHEN q.bytes_limit = 0 THEN 9223372036854775807
		           ELSE (q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(m.inflight, 0))
		      END >= ?
		ORDER BY CASE WHEN q.bytes_limit = 0 THEN 0.0
		              ELSE CAST(q.bytes_used + q.orphan_bytes AS REAL) / CAST(q.bytes_limit AS REAL)
		         END ASC
		LIMIT 1`

	args := toArgs(eligible)
	args = append(args, size)

	var backendName string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&backendName)
	if err == sql.ErrNoRows {
		return "", store.ErrNoSpaceAvailable
	}
	if err != nil {
		return "", fmt.Errorf("failed to find least utilized backend: %w", err)
	}
	return backendName, nil
}

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for i := range backends {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO backend_quotas (backend_name, bytes_limit, bytes_used, updated_at)
				VALUES (?, ?, 0, ?)
				ON CONFLICT (backend_name) DO UPDATE SET
					bytes_limit = excluded.bytes_limit,
					updated_at = excluded.updated_at`,
				backends[i].Name, backends[i].QuotaBytes, now); err != nil {
				return fmt.Errorf("failed to sync quota for backend %s: %w", backends[i].Name, err)
			}
		}
		return nil
	})
}

// GetQuotaStats returns quota statistics for all backends.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]store.QuotaStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, bytes_used, bytes_limit, orphan_bytes, updated_at
		FROM backend_quotas`)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[string]store.QuotaStat)
	for rows.Next() {
		var qs store.QuotaStat
		var updatedAt string
		if err := rows.Scan(&qs.BackendName, &qs.BytesUsed, &qs.BytesLimit, &qs.OrphanBytes, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan quota stat: %w", err)
		}
		qs.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		stats[qs.BackendName] = qs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate quota stats: %w", err)
	}
	return stats, nil
}

// GetObjectCounts returns the number of objects stored on each backend.
func (s *Store) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, COUNT(*) AS object_count
		FROM object_locations
		GROUP BY backend_name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query object counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("failed to scan object count: %w", err)
		}
		counts[name] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate object counts: %w", err)
	}
	return counts, nil
}

// IncrementOrphanBytes adds bytes to the orphan_bytes counter for a backend.
// Called when a physical delete fails and is enqueued for retry.
func (s *Store) IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = orphan_bytes + ?, updated_at = ?
		WHERE backend_name = ?`, amount, now, backendName)
	if err != nil {
		return fmt.Errorf("failed to increment orphan bytes: %w", err)
	}
	return nil
}

// DecrementOrphanBytes subtracts bytes from the orphan_bytes counter for a
// backend. Called when a cleanup queue item is successfully processed or
// exhausted. Uses MAX(0, x-y) instead of PostgreSQL GREATEST to prevent
// underflow.
func (s *Store) DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = MAX(0, orphan_bytes - ?), updated_at = ?
		WHERE backend_name = ?`, amount, now, backendName)
	if err != nil {
		return fmt.Errorf("failed to decrement orphan bytes: %w", err)
	}
	return nil
}

// FlushUsageDeltas atomically adds accumulated usage deltas to the persistent
// usage row. Creates the row if it doesn't exist for this (backend, period).
func (s *Store) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO backend_usage (backend_name, period, api_requests, egress_bytes, ingress_bytes, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (backend_name, period) DO UPDATE SET
			api_requests  = backend_usage.api_requests  + excluded.api_requests,
			egress_bytes  = backend_usage.egress_bytes  + excluded.egress_bytes,
			ingress_bytes = backend_usage.ingress_bytes + excluded.ingress_bytes,
			updated_at    = excluded.updated_at`,
		backendName, period, apiRequests, egressBytes, ingressBytes, now)
	if err != nil {
		return fmt.Errorf("failed to flush usage deltas: %w", err)
	}
	return nil
}

// GetUsageForPeriod returns usage statistics for all backends in the given period.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]store.UsageStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, api_requests, egress_bytes, ingress_bytes
		FROM backend_usage
		WHERE period = ?`, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]store.UsageStat)
	for rows.Next() {
		var name string
		var us store.UsageStat
		if err := rows.Scan(&name, &us.APIRequests, &us.EgressBytes, &us.IngressBytes); err != nil {
			return nil, fmt.Errorf("failed to scan usage stat: %w", err)
		}
		stats[name] = us
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate usage stats: %w", err)
	}
	return stats, nil
}

// placeholders returns a string of n comma-separated "?" placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// toArgs converts a string slice to a slice of any for use as query arguments.
func toArgs(ss []string) []any {
	args := make([]any, len(ss))
	for i, s := range ss {
		args[i] = s
	}
	return args
}
