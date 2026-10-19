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

// ApplyUsageBatch adds a batch's totals and pools to its period's rows in one
// transaction. A batch with an ID records it first, and one already recorded
// is left alone; see core.UsageFlusher.
func (s *Store) ApplyUsageBatch(ctx context.Context, b *core.UsageBatch) error {
	now := now()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if fresh, err := recordUsageFlush(ctx, tx, b.ID); err != nil || !fresh {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO backend_usage (backend_name, period, api_requests, egress_bytes, ingress_bytes, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (backend_name, period) DO UPDATE SET
				api_requests  = backend_usage.api_requests  + excluded.api_requests,
				egress_bytes  = backend_usage.egress_bytes  + excluded.egress_bytes,
				ingress_bytes = backend_usage.ingress_bytes + excluded.ingress_bytes,
				updated_at    = excluded.updated_at`,
			b.Backend, b.Period, b.APIRequests, b.EgressBytes, b.IngressBytes, now); err != nil {
			return fmt.Errorf("failed to flush usage deltas: %w", err)
		}
		for pool, requests := range b.Pools {
			if requests == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO backend_request_usage (backend_name, period, pool, requests, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (backend_name, period, pool) DO UPDATE SET
					requests   = backend_request_usage.requests + excluded.requests,
					updated_at = excluded.updated_at`,
				b.Backend, b.Period, pool, requests, now); err != nil {
				return fmt.Errorf("failed to flush pool deltas: %w", err)
			}
		}
		return nil
	})
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

// GetUsageForPeriod returns every backend's totals and pool counts for the
// given period, keyed by backend name. One query: each backend's totals row
// joined to its pool rows, so a backend with pools repeats its totals.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]core.Usage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.backend_name, u.api_requests, u.egress_bytes, u.ingress_bytes, p.pool, p.requests
		FROM backend_usage u
		LEFT JOIN backend_request_usage p ON p.backend_name = u.backend_name AND p.period = u.period
		WHERE u.period = ?`, period)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	usage := make(map[string]core.Usage)
	for rows.Next() {
		var (
			name     string
			u        core.UsageStat
			pool     sql.NullString
			requests sql.NullInt64
		)
		if err := rows.Scan(&name, &u.APIRequests, &u.EgressBytes, &u.IngressBytes, &pool, &requests); err != nil {
			return nil, fmt.Errorf("failed to scan usage: %w", err)
		}
		entry := usage[name]
		entry.UsageStat = u
		if pool.Valid {
			if entry.Pools == nil {
				entry.Pools = make(core.PoolUsage)
			}
			entry.Pools[pool.String] = requests.Int64
		}
		usage[name] = entry
	}
	return usage, rows.Err()
}
