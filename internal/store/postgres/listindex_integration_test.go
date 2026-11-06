//go:build integration

// -------------------------------------------------------------------------------
// Prefix Listing Plan Shape
//
// Author: Alex Freidah
//
// Pins that ListObjectsByPrefix plans an index-only scan with no sort against
// the listing index, and that both listings keep their key range in the index
// under a generic plan. These are easy to lose by accident: adding a column to
// the projection without adding it to the index's INCLUDE list drops the plan
// back to heap fetches per replica row walked, moving created_at out of the index
// key reintroduces a per-group sort, and a bound the generic plan cannot use
// scans to the end of the index. None of them shows up as a test failure
// anywhere else, only as a slower listing.
//
// The plans are taken from the statements the generated bindings send, so they
// cannot drift from what production runs.
//
// The package shares one Postgres fixture, so the seed lives under its own key
// prefix and backends and is removed again on cleanup. Tests that assert their
// key appears in a bounded scan of object_locations fail if these rows outlive
// the test that needs them.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// errStatementCaptured stops a generated query once the stub has its statement.
var errStatementCaptured = errors.New("statement captured")

// listPlanPrefix namespaces the seeded rows, and listPlanBackend their quota
// rows, so both can be deleted without touching what other tests wrote.
const (
	listPlanPrefix  = "listplan/"
	listPlanBackend = "listplan-backend-"
)

// TestListObjectsByPrefix_PlansIndexOnlyWithoutSort seeds enough copies for the
// planner to prefer an index scan and asserts the shape of the plan it picks.
func TestListObjectsByPrefix_PlansIndexOnlyWithoutSort(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const (
		keys   = 50000
		copies = 3
	)
	seedListingRows(ctx, t, s, keys, copies)

	plan := explainListing(ctx, t, s)

	if !strings.Contains(plan, "Index Only Scan using idx_object_locations_listing") {
		t.Errorf("listing does not plan an index-only scan on the listing index; every replica row walked costs a heap fetch.\n%s", plan)
	}
	// Covers "Sort" and "Incremental Sort" alike: the index supplies
	// (object_key, created_at), so DISTINCT ON needs no sort to pick a winner.
	if strings.Contains(plan, "Sort") {
		t.Errorf("listing plans a sort; created_at must be an index key column, not INCLUDE payload.\n%s", plan)
	}
	// Not zero: the fixture is shared, so a few pages of the seed are never
	// all-visible however hard the seed vacuums, and exact zero would fail on
	// the visibility map rather than on the index. A projection the index does
	// not carry costs a fetch per row walked, which lands far above this.
	fetches, walked := heapFetches(t, plan)
	if fetches*20 > walked {
		t.Errorf("listing takes %d heap fetches over %d rows walked; the index is missing a projected column.\n%s",
			fetches, walked, plan)
	}
}

// TestListings_KeepTheirRangeUnderTheGenericPlan asks for the plan a pooled
// connection moves to after five executions, which sees no parameter values. A
// bound that plan cannot put in the index turns into a filter, and the listing
// reads past its prefix to the end of the index.
func TestListings_KeepTheirRangeUnderTheGenericPlan(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	seedListingRows(ctx, t, s, 50000, 3)
	statements := map[string]func(*db.Queries) error{
		"flat": func(q *db.Queries) error {
			_, err := q.ListObjectsByPrefix(ctx, db.ListObjectsByPrefixParams{})
			return err
		},
		"delimited": func(q *db.Queries) error {
			_, err := q.ListObjectsDelimited(ctx, db.ListObjectsDelimitedParams{})
			return err
		},
	}
	for name, run := range statements {
		t.Run(name, func(t *testing.T) {
			query, _ := generatedStatement(t, run)
			plan := genericPlan(ctx, t, s, query)
			if !strings.Contains(plan, "Index Cond") {
				t.Errorf("no index condition.\n%s", plan)
			}
			for line := range strings.Lines(plan) {
				if strings.Contains(line, "Filter") && strings.Contains(line, "object_key") {
					t.Errorf("a key bound is a filter, so the scan runs past the prefix: %s\n%s", line, plan)
				}
			}
		})
	}
}

// generatedStatement returns the SQL and arguments run sends through the
// generated bindings, which is what a plan test has to measure.
func generatedStatement(t *testing.T, run func(*db.Queries) error) (string, []any) {
	t.Helper()
	stub := &stubDBTX{queryErr: errStatementCaptured, execErr: errStatementCaptured}
	if err := run(db.New(stub)); !errors.Is(err, errStatementCaptured) {
		t.Fatalf("generated query = %v, want it stopped at the stub", err)
	}
	return stub.querySQL, stub.queryArgs
}

// genericPlan returns the generic plan of query. The simple protocol carries the
// statement with its parameters unbound, which pgx's own query path refuses.
func genericPlan(ctx context.Context, t *testing.T, s *Store, query string) string {
	t.Helper()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	results, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, COSTS OFF) "+query).ReadAll()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan strings.Builder
	for _, result := range results {
		for _, row := range result.Rows {
			plan.Write(row[0])
			plan.WriteByte('\n')
		}
	}
	return plan.String()
}

// heapFetches pulls the heap-fetch count and the rows the index scan walked
// out of the plan text.
func heapFetches(t *testing.T, plan string) (fetches, walked int) {
	t.Helper()
	match := func(re *regexp.Regexp) int {
		m := re.FindStringSubmatch(plan)
		if m == nil {
			t.Fatalf("plan does not report %s:\n%s", re, plan)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse %s: %v", re, err)
		}
		return n
	}
	return match(regexp.MustCompile(`Heap Fetches: (\d+)`)),
		match(regexp.MustCompile(`Index Only Scan[^\n]*actual rows=(\d+)`))
}

// seedListingRows fills object_locations with one row per copy, every copy of a
// key sharing created_at as the write paths guarantee, and removes them again
// when the test ends.
func seedListingRows(ctx context.Context, t *testing.T, s *Store, keys, copies int) {
	t.Helper()
	t.Cleanup(func() { dropListingRows(t, s) })

	// backend_name is a foreign key, so the copies need quota rows to point at.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO backend_quotas (backend_name, bytes_limit)
		SELECT $2::text || c, 1 << 40 FROM generate_series(1, $1) c
		ON CONFLICT (backend_name) DO NOTHING`, copies, listPlanBackend); err != nil {
		t.Fatalf("seed backends: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO object_locations
		    (object_key, storage_key, backend_name, size_bytes, etag, content_type, managed, created_at)
		SELECT
		    key, key,
		    $4::text || c,
		    (k % 5000000)::bigint,
		    md5(k::text),
		    'image/jpeg',
		    true,
		    NOW() - (k || ' seconds')::interval
		FROM generate_series(1, $1) k
		CROSS JOIN generate_series(1, $2) c
		CROSS JOIN LATERAL (
		    SELECT format('%s%s/IMG_%s.jpg', $3::text, to_char(DATE '2024-01-01' + (k % 900), 'YYYY/MM'),
		                  lpad(k::text, 8, '0')) AS key
		) named`,
		keys, copies, listPlanPrefix, listPlanBackend); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	// Both halves matter: ANALYZE so the planner sees the real distribution,
	// VACUUM so the visibility map allows an index-only scan at all.
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE object_locations`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
}

// dropListingRows removes the seed. The VACUUM ANALYZE matters as much as the
// deletes: it hands the next test a table whose statistics describe what is
// actually in it rather than the 150 K rows that were.
func dropListingRows(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM object_locations WHERE object_key LIKE $1 || '%'`, listPlanPrefix); err != nil {
		t.Fatalf("drop seeded rows: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM backend_quotas WHERE backend_name LIKE $1 || '%'`, listPlanBackend); err != nil {
		t.Fatalf("drop seeded backends: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE object_locations`); err != nil {
		t.Fatalf("vacuum analyze after cleanup: %v", err)
	}
}

// explainListing returns the plan for one page of the prefix listing.
func explainListing(ctx context.Context, t *testing.T, s *Store) string {
	t.Helper()
	query, args := generatedStatement(t, func(q *db.Queries) error {
		_, err := q.ListObjectsByPrefix(ctx, db.ListObjectsByPrefixParams{
			FromKey: listPlanPrefix, EndKey: core.PrefixEnd(listPlanPrefix), MaxKeys: 1000,
		})
		return err
	})
	return explainPlan(ctx, t, s.pool, "ANALYZE, BUFFERS, COSTS OFF, TIMING OFF", query, args...)
}
