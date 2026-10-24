// -------------------------------------------------------------------------------
// SQLite Quota and Usage - Backend Space Management and Usage Tracking
//
// Author: Alex Freidah
//
// Implements the SQLite quota and usage bindings: per-backend limits, the
// capacity snapshot, the usage drift reconcile, the ledger figures, and the
// usage flush and its reads.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

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

// GetQuotaStats returns each backend's ceiling and the byte totals a write is
// judged against, keyed by backend. The figures come from backend_capacity,
// the same view admission tests against.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_name, bytes_limit, bytes_used, orphan_bytes, inflight_bytes
		FROM backend_capacity`)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}
	return collectMap(rows, "quota stats", func(rows *sql.Rows) (string, core.QuotaStat, error) {
		var qs core.QuotaStat
		if err := rows.Scan(&qs.BackendName, &qs.BytesLimit, &qs.BytesUsed, &qs.OrphanBytes, &qs.InflightBytes); err != nil {
			return "", core.QuotaStat{}, fmt.Errorf("failed to scan quota stat: %w", err)
		}
		return qs.BackendName, qs, nil
	})
}

// ReconcileUsage brings each backend's striped total to the ledger's sum of its
// copies and returns the difference applied, for the backends that had drifted.
// The difference is added to stripe zero rather than written over the total,
// and the read and the writes share one transaction, which SQLite serializes
// against every other write. A backend with no copies left is brought to zero.
func (s *Store) ReconcileUsage(ctx context.Context) (map[string]int64, error) {
	var drift map[string]int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		a := &sqliteTxAdapter{q: tx}
		rows, err := tx.QueryContext(ctx, `
			WITH ledger AS (
				SELECT backend_name, SUM(size_bytes) AS total
				FROM object_locations
				GROUP BY backend_name
			),
			striped AS (
				SELECT q.backend_name, COALESCE(SUM(s.bytes_used), 0) AS total
				FROM backend_quotas q
				LEFT JOIN backend_quota_stripes s ON s.backend_name = q.backend_name
				GROUP BY q.backend_name
			)
			SELECT st.backend_name, COALESCE(l.total, 0) - st.total
			FROM striped st
			LEFT JOIN ledger l ON l.backend_name = st.backend_name
			WHERE COALESCE(l.total, 0) <> st.total`)
		if err != nil {
			return fmt.Errorf("read usage drift: %w", err)
		}
		if drift, err = collectMap(rows, "usage drift", scanNameValue); err != nil {
			return err
		}
		for backend, delta := range drift {
			if err := a.AdjustQuotaStripe(ctx, backend, 0, delta); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return drift, nil
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

// ApplyUsageBatch adds each workload's totals and pools in a batch to its
// buckets' rows in one transaction, then drops the backend's buckets older
// than core.UsageRetention. A batch with an ID records it first, and one
// already recorded is left alone; see core.UsageFlusher.
func (s *Store) ApplyUsageBatch(ctx context.Context, b *core.UsageBatch) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if fresh, err := recordUsageFlush(ctx, tx, b.ID); err != nil || !fresh {
			return err
		}
		if err := b.Each(func(bucket time.Time, workload string, u *core.Usage) error {
			return applyWorkloadUsage(ctx, tx, b.Backend, formatTime(bucket), workload, u)
		}); err != nil {
			return err
		}
		return pruneUsageBuckets(ctx, tx, b.Backend)
	})
}

// pruneUsageBuckets drops one backend's buckets older than the retention
// window, inside the flush's transaction.
func pruneUsageBuckets(ctx context.Context, tx *sql.Tx, backend string) error {
	cutoff := formatTime(time.Now().Add(-core.UsageRetention))
	for table, query := range map[string]string{
		"backend_usage":         `DELETE FROM backend_usage WHERE backend_name = ? AND bucket_start < ?`,
		"backend_request_usage": `DELETE FROM backend_request_usage WHERE backend_name = ? AND bucket_start < ?`,
	} {
		if _, err := tx.ExecContext(ctx, query, backend, cutoff); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
	}
	return nil
}

// applyWorkloadUsage adds one workload's totals and pools to its bucket's
// rows, inside the batch's transaction.
func applyWorkloadUsage(ctx context.Context, tx *sql.Tx, backend, bucket, workload string, u *core.Usage) error {
	now := now()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO backend_usage (backend_name, bucket_start, workload, api_requests, egress_bytes, ingress_bytes, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (backend_name, bucket_start, workload) DO UPDATE SET
			api_requests  = backend_usage.api_requests  + excluded.api_requests,
			egress_bytes  = backend_usage.egress_bytes  + excluded.egress_bytes,
			ingress_bytes = backend_usage.ingress_bytes + excluded.ingress_bytes,
			updated_at    = excluded.updated_at`,
		backend, bucket, workload, u.APIRequests, u.EgressBytes, u.IngressBytes, now); err != nil {
		return fmt.Errorf("failed to flush usage deltas: %w", err)
	}
	for pool, requests := range u.Pools {
		if requests == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO backend_request_usage (backend_name, bucket_start, workload, pool, requests, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (backend_name, bucket_start, workload, pool) DO UPDATE SET
				requests   = backend_request_usage.requests + excluded.requests,
				updated_at = excluded.updated_at`,
			backend, bucket, workload, pool, requests, now); err != nil {
			return fmt.Errorf("failed to flush pool deltas: %w", err)
		}
	}
	return nil
}

// usageFlushRetention is how long a shared batch's id is kept, past the 35
// days its staged copy can live in Redis.
const usageFlushRetention = 40 * 24 * time.Hour

// recordUsageFlush records a shared batch's id and prunes old ones, reporting
// whether the batch is new and should be applied. A batch with no id is
// always new.
func recordUsageFlush(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO usage_flushes (flush_id, applied_at) VALUES (?, ?)
		ON CONFLICT (flush_id) DO NOTHING`, id, now())
	if err != nil {
		return false, fmt.Errorf("record usage flush: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_flushes WHERE applied_at < ?`,
		formatTime(time.Now().Add(-usageFlushRetention))); err != nil {
		return false, fmt.Errorf("prune usage flushes: %w", err)
	}
	return true, nil
}

// GetUsageSince sums each backend's buckets from its own start onward, by
// workload, from one query: each workload's summed totals joined to its pool
// sums, so a workload with pools repeats its totals. See core.UsageReader.
func (s *Store) GetUsageSince(ctx context.Context, starts map[string]time.Time) (map[string]core.WorkloadUsage, error) {
	usage := make(map[string]core.WorkloadUsage)
	if len(starts) == 0 {
		return usage, nil
	}
	values := make([]string, 0, len(starts))
	args := make([]any, 0, 2*len(starts))
	for backend, start := range starts {
		values = append(values, "(?, ?)")
		args = append(args, backend, formatTime(start))
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH win(backend_name, start) AS (VALUES `+strings.Join(values, ", ")+`),
		totals AS (
			SELECT u.backend_name, u.workload, SUM(u.api_requests) AS api_requests,
			       SUM(u.egress_bytes) AS egress_bytes, SUM(u.ingress_bytes) AS ingress_bytes
			FROM backend_usage u
			JOIN win ON win.backend_name = u.backend_name AND u.bucket_start >= win.start
			GROUP BY u.backend_name, u.workload
		),
		pools AS (
			SELECT p.backend_name, p.workload, p.pool, SUM(p.requests) AS requests
			FROM backend_request_usage p
			JOIN win ON win.backend_name = p.backend_name AND p.bucket_start >= win.start
			GROUP BY p.backend_name, p.workload, p.pool
		)
		SELECT t.backend_name, t.workload, t.api_requests, t.egress_bytes, t.ingress_bytes, p.pool, p.requests
		FROM totals t
		LEFT JOIN pools p ON p.backend_name = t.backend_name AND p.workload = t.workload`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			name, workload string
			stat           core.UsageStat
			pool           *string
			requests       *int64
		)
		if err := rows.Scan(&name, &workload, &stat.APIRequests, &stat.EgressBytes, &stat.IngressBytes, &pool, &requests); err != nil {
			return nil, fmt.Errorf("failed to scan usage: %w", err)
		}
		core.AddUsageRow(usage, name, workload, stat, pool, requests)
	}
	return usage, rows.Err()
}
