// -------------------------------------------------------------------------------
// Postgres Usage Buckets Migration Integration Test
//
// Author: Alex Freidah
//
// The upgrade to hourly buckets carries each monthly usage row over as one
// bucket at the start of its month, so a cycle in progress keeps what it has
// spent. Rolling back folds the buckets into monthly rows again.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"
)

// monthlyUsageVersion is the last schema that keyed usage by a monthly period
// label.
const monthlyUsageVersion = 46

// TestMigrations_UsageBucketsKeepMonthlyUsage verifies monthly rows read back
// from their month's start after the upgrade and not from any later hour, and
// that a rollback sums the month's buckets into one row again.
func TestMigrations_UsageBucketsKeepMonthlyUsage(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := freshDatabase(ctx, t)
	migrateTo(ctx, t, s.connStr, monthlyUsageVersion)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO backend_quotas (backend_name, bytes_limit) VALUES ('b1', 0);
		INSERT INTO backend_usage (backend_name, period, workload, api_requests) VALUES ('b1', '2026-09', 'client', 40);
		INSERT INTO backend_request_usage (backend_name, period, workload, pool, requests) VALUES ('b1', '2026-09', 'client', 'reads', 40);`); err != nil {
		t.Fatalf("seed monthly usage: %v", err)
	}

	migrateTo(ctx, t, s.connStr, ExpectedSchemaVersion)

	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	usage, err := s.GetUsageSince(ctx, map[string]time.Time{"b1": month})
	if err != nil {
		t.Fatalf("GetUsageSince: %v", err)
	}
	if u := usage["b1"].Total(); u.APIRequests != 40 || u.Pools["reads"] != 40 {
		t.Errorf("usage from the month start = %+v, want the carried 40 and reads=40", u)
	}
	if later, err := s.GetUsageSince(ctx, map[string]time.Time{"b1": month.Add(time.Hour)}); err != nil || len(later) != 0 {
		t.Errorf("usage after the month's first hour = %v, %v; want none", later, err)
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO backend_usage (backend_name, bucket_start, workload, api_requests)
		VALUES ('b1', '2026-09-20T05:00:00Z', 'client', 2)`); err != nil {
		t.Fatalf("seed a later bucket: %v", err)
	}
	migrateDownTo(ctx, t, s.connStr, monthlyUsageVersion)
	var period string
	var api int64
	if err := s.pool.QueryRow(ctx,
		`SELECT period, api_requests FROM backend_usage WHERE backend_name = 'b1'`).Scan(&period, &api); err != nil {
		t.Fatalf("read rolled-back usage: %v", err)
	}
	if period != "2026-09" || api != 42 {
		t.Errorf("rolled back to %s with %d requests, want 2026-09 with 42", period, api)
	}
}
