// -------------------------------------------------------------------------------
// Backend Removal Check
//
// Author: Alex Freidah
//
// Removing a backend ends with deleting its backend_quotas row, which every
// table holding per-backend rows references. A table the removal skips makes
// that delete fail on the foreign key, so the check seeds a row in every
// referencing table the engine's catalog reports, removes the backend, and
// counts what is left. A table that gains the reference fails here until it
// has a seed, which is also when the removal has to learn to clear it.
// -------------------------------------------------------------------------------

package storetest

import (
	"maps"
	"slices"
	"testing"
)

// backendRowSeeds inserts one row for the backend named by $1 into each table
// that references backend_quotas. Both engines accept the same statements.
var backendRowSeeds = map[string]string{
	"backend_quota_stripes": `INSERT INTO backend_quota_stripes (backend_name, stripe_id) VALUES ($1, 999)`,
	"object_locations":      `INSERT INTO object_locations (object_key, backend_name, storage_key, size_bytes) VALUES ($1, $1, $1, 1)`,
	"multipart_uploads":     `INSERT INTO multipart_uploads (upload_id, object_key, backend_name) VALUES ($1, $1, $1)`,
	"backend_usage":         `INSERT INTO backend_usage (backend_name, bucket_start, workload) VALUES ($1, '2026-10-01T00:00:00Z', 'client')`,
	"backend_request_usage": `INSERT INTO backend_request_usage (backend_name, bucket_start, workload, pool) VALUES ($1, '2026-10-01T00:00:00Z', 'client', 'reads')`,
	"cleanup_queue":         `INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason) VALUES ($1, $1, $1, 'test')`,
	"cleanup_dlq":           `INSERT INTO cleanup_dlq (original_id, backend_name, object_key, storage_key, reason, attempts, first_enqueued_at) VALUES (1, $1, $1, $1, 'test', 10, '2026-10-19T00:00:00Z')`,
	"pending_objects":       `INSERT INTO pending_objects (intent_id, object_key, storage_key, backend_name, size_bytes) VALUES ($1, $1, $1, $1, 1)`,
}

// BackendRemoval is one engine's side of the check. Referencing lists the
// tables with a foreign key to backend_quotas, read from the catalog. Exec
// runs a statement and Count returns a single-row count, both with $1 bound
// to the backend. Remove deletes the backend's data.
type BackendRemoval struct {
	Referencing []string
	Exec        func(query, backend string) error
	Count       func(query, backend string) (int64, error)
	Remove      func(backend string) error
}

// CheckBackendRemoval seeds removed and kept in every referencing table,
// removes removed, and fails unless none of its rows remain and all of kept's
// do. Both backends must already have their backend_quotas row.
func CheckBackendRemoval(t *testing.T, r BackendRemoval, removed, kept string) {
	t.Helper()
	seeded := slices.Sorted(maps.Keys(backendRowSeeds))
	if got := slices.Sorted(slices.Values(r.Referencing)); !slices.Equal(got, seeded) {
		t.Fatalf("tables referencing backend_quotas = %v, seeded %v; seed each one and clear it on backend removal", got, seeded)
	}
	for table, seed := range backendRowSeeds {
		for _, backend := range []string{removed, kept} {
			if err := r.Exec(seed, backend); err != nil {
				t.Fatalf("seed %s for %s: %v", table, backend, err)
			}
		}
	}

	if err := r.Remove(removed); err != nil {
		t.Fatalf("remove %s: %v", removed, err)
	}

	for _, table := range seeded {
		r.checkRows(t, table, removed, false)
		r.checkRows(t, table, kept, true)
	}
}

// checkRows fails the test unless the table holds rows for the backend
// exactly when want is true.
func (r BackendRemoval) checkRows(t *testing.T, table, backend string, want bool) {
	t.Helper()
	query := "SELECT COUNT(*) FROM " + table + " WHERE backend_name = $1" //nolint:gosec // G202: table name from a fixed seed list
	n, err := r.Count(query, backend)
	if err != nil {
		t.Fatalf("count %s for %s: %v", table, backend, err)
	}
	if (n > 0) != want {
		t.Errorf("%s has %d rows for %s after removal", table, n, backend)
	}
}
