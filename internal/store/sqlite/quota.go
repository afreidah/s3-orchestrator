// -------------------------------------------------------------------------------
// SQLite Quota and Usage - Backend Space Management and Usage Tracking
//
// Author: Alex Freidah
//
// Implements quota enforcement, backend space selection and usage delta
// flushing for the SQLite backend. Uses dynamic IN clause expansion for
// backend-filtered queries.
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
				INSERT INTO backend_quotas (backend_name, bytes_limit, updated_at)
				VALUES (?, ?, ?)
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
		SELECT c.backend_name, c.bytes_used, c.bytes_limit, c.orphan_bytes, q.updated_at
		FROM backend_capacity c
		JOIN backend_quotas q ON q.backend_name = c.backend_name`)
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
// write is judged against, for the quota tracker's baseline refresh. The figures
// come from backend_capacity, the same view admission tests against.
func (s *Store) ListBackendQuotaUsage(ctx context.Context) ([]core.BackendQuotaUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, bytes_limit, bytes_used, orphan_bytes, inflight_bytes
		FROM backend_capacity`)
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

// LedgerStats returns every per-backend ledger figure from one pass over
// object_locations grouped by backend, stored form and managed flag, folded by
// core. The oldest touch falls back to created_at as the scrub queue ordering
// does.
func (s *Store) LedgerStats(ctx context.Context) (core.LedgerStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, form, managed,
		       COUNT(*),
		       COALESCE(SUM(size_bytes), 0),
		       COALESCE(SUM(object_size), 0),
		       COUNT(*) FILTER (WHERE last_scrubbed_at IS NULL),
		       MIN(COALESCE(last_scrubbed_at, created_at))
		FROM object_locations
		GROUP BY backend_name, form, managed`)
	if err != nil {
		return nil, fmt.Errorf("failed to query ledger stats: %w", err)
	}
	groups, err := collectRows(rows, "ledger stats", scanLedgerGroup)
	if err != nil {
		return nil, err
	}
	return core.FoldLedger(groups), nil
}

// scanLedgerGroup reads one row of the grouped ledger.
func scanLedgerGroup(rows *sql.Rows) (core.LedgerGroup, error) {
	var (
		g      core.LedgerGroup
		oldest sql.NullString
	)
	if err := rows.Scan(&g.Backend, &g.Form, &g.Managed, &g.Copies, &g.StoredBytes,
		&g.ObjectBytes, &g.NeverScrubbed, &oldest); err != nil {
		return core.LedgerGroup{}, fmt.Errorf("scan ledger stats: %w", err)
	}
	if oldest.Valid {
		t, err := parseTime(oldest.String)
		if err != nil {
			return core.LedgerGroup{}, fmt.Errorf("parse oldest touch %q: %w", oldest.String, err)
		}
		g.OldestTouched = t
	}
	return g, nil
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
// their persistent rows in one transaction. A failure writes none of them, so
// the caller can restore every delta to the counter without double-counting.
func (s *Store) FlushPoolDeltas(ctx context.Context, backendName, period string, deltas core.PoolUsage) error {
	now := now()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for pool, requests := range deltas {
			if requests == 0 {
				continue
			}
			_, err := tx.ExecContext(ctx, `
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
	})
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
