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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// QUOTA ADMIN AND STATS
// -------------------------------------------------------------------------

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := now()
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
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, bytes_used, bytes_limit, orphan_bytes, updated_at
		FROM backend_quotas`)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}
	return collectMap(rows, "quota stats", func(rows *sql.Rows) (string, core.QuotaStat, error) {
		var (
			qs        core.QuotaStat
			updatedAt string
		)
		if err := rows.Scan(&qs.BackendName, &qs.BytesUsed, &qs.BytesLimit, &qs.OrphanBytes, &updatedAt); err != nil {
			return "", core.QuotaStat{}, fmt.Errorf("failed to scan quota stat: %w", err)
		}
		parsed, err := parseTime(updatedAt)
		if err != nil {
			return "", core.QuotaStat{}, fmt.Errorf("invalid updated_at timestamp %q: %w", updatedAt, err)
		}
		qs.UpdatedAt = parsed
		return qs.BackendName, qs, nil
	})
}

// ListBackendQuotaUsage returns each backend's ceiling and the byte totals a
// write is judged against, for the quota tracker's baseline refresh. The
// in-flight join mirrors GetBackendWithSpace: parts of uploads that have not
// completed occupy the backend without appearing in bytes_used.
func (s *Store) ListBackendQuotaUsage(ctx context.Context) ([]core.BackendQuotaUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT q.backend_name, q.bytes_limit, q.bytes_used, q.orphan_bytes,
		       COALESCE(m.inflight, 0) AS inflight_bytes
		FROM backend_quotas q
		LEFT JOIN (
			SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
			FROM multipart_uploads mu
			JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
			GROUP BY mu.backend_name
		) m ON m.backend_name = q.backend_name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query backend quota usage: %w", err)
	}
	return collectRows(rows, "backend quota usage", func(rows *sql.Rows) (core.BackendQuotaUsage, error) {
		var u core.BackendQuotaUsage
		if err := rows.Scan(&u.BackendName, &u.BytesLimit, &u.BytesUsed, &u.OrphanBytes, &u.InflightBytes); err != nil {
			return core.BackendQuotaUsage{}, fmt.Errorf("failed to scan backend quota usage: %w", err)
		}
		return u, nil
	})
}

// GetObjectCounts returns the number of objects stored on each backend.
func (s *Store) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	return s.countObjectsByBackend(ctx, "", "object counts")
}

// GetUnverifiedObjectCounts returns the number of objects per backend
// whose content_hash column is NULL (objects predating integrity
// verification or otherwise not yet checksummed). Drives the dashboard's
// "needs backfill" column.
func (s *Store) GetUnverifiedObjectCounts(ctx context.Context) (map[string]int64, error) {
	return s.countObjectsByBackend(ctx, "WHERE content_hash IS NULL", "unverified counts")
}

// countObjectsByBackend runs a per-backend COUNT(*) aggregation on
// object_locations and returns the map. The whereClause is appended
// verbatim (callers control the predicate); errLabel feeds the wrapped
// error string so failures stay attributable to the calling helper.
// Static SQL only - no caller-supplied strings interpolated beyond the
// fixed clauses defined in this file.
func (s *Store) countObjectsByBackend(ctx context.Context, whereClause, errLabel string) (map[string]int64, error) {
	query := "SELECT backend_name, COUNT(*) FROM object_locations " + whereClause + " GROUP BY backend_name"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query %s: %w", errLabel, err)
	}
	return collectMap(rows, errLabel, scanNameValue)
}

// -------------------------------------------------------------------------
// ORPHAN BYTES
// -------------------------------------------------------------------------

// IncrementOrphanBytes adds bytes to the orphan_bytes counter for a backend.
// Called when a physical delete fails and is enqueued for retry.
func (s *Store) IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error {
	now := now()
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
	now := now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE backend_quotas
		SET orphan_bytes = MAX(0, orphan_bytes - ?), updated_at = ?
		WHERE backend_name = ?`, amount, now, backendName)
	if err != nil {
		return fmt.Errorf("failed to decrement orphan bytes: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// USAGE DELTAS
// -------------------------------------------------------------------------

// FlushUsageDeltas atomically adds accumulated usage deltas to the persistent
// usage row. Creates the row if it doesn't exist for this (backend, period).
func (s *Store) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	now := now()
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

// FlushPoolDeltas adds one backend's accumulated per-pool request counts to
// their persistent rows, one statement per pool so a single bad pool name does
// not fail the whole flush.
func (s *Store) FlushPoolDeltas(ctx context.Context, backendName, period string, deltas core.PoolUsage) error {
	now := now()
	for pool, requests := range deltas {
		if requests == 0 {
			continue
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO backend_request_usage (backend_name, period, pool, requests, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (backend_name, period, pool) DO UPDATE SET
				requests   = backend_request_usage.requests + excluded.requests,
				updated_at = excluded.updated_at`,
			backendName, period, pool, requests, now)
		if err != nil {
			return fmt.Errorf("failed to flush pool deltas: %w", err)
		}
	}
	return nil
}

// GetPoolUsageForPeriod returns every backend's per-pool request counts for
// the given period, keyed by backend name.
func (s *Store) GetPoolUsageForPeriod(ctx context.Context, period string) (map[string]core.PoolUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, pool, requests
		FROM backend_request_usage
		WHERE period = ?`, period)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	usage := make(map[string]core.PoolUsage)
	for rows.Next() {
		var (
			name     string
			pool     string
			requests int64
		)
		if err := rows.Scan(&name, &pool, &requests); err != nil {
			return nil, fmt.Errorf("failed to scan pool usage: %w", err)
		}
		if usage[name] == nil {
			usage[name] = make(core.PoolUsage)
		}
		usage[name][pool] = requests
	}
	return usage, rows.Err()
}

// GetUsageForPeriod returns usage statistics for all backends in the given period.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.UsageStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, api_requests, egress_bytes, ingress_bytes
		FROM backend_usage
		WHERE period = ?`, period)
	if err != nil {
		return nil, err
	}
	return collectMap(rows, "usage stats", func(rows *sql.Rows) (string, core.UsageStat, error) {
		var (
			name string
			us   core.UsageStat
		)
		if err := rows.Scan(&name, &us.APIRequests, &us.EgressBytes, &us.IngressBytes); err != nil {
			return "", core.UsageStat{}, fmt.Errorf("failed to scan usage stat: %w", err)
		}
		return name, us, nil
	})
}
