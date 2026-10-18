// -------------------------------------------------------------------------------
// Postgres Derived Orphan Bytes Migration Integration Test
//
// Author: Alex Freidah
//
// Upgrades a database that still kept orphan bytes as a counter: after the
// upgrade backend_capacity derives them from the queued and dead-lettered
// cleanup rows, whatever the counter held, and multipart parts gain a path
// column. Rolling back restores the counter from those rows.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// counterSchemaVersion is the last schema that kept orphan bytes as a counter
// on backend_quotas.
const counterSchemaVersion = 39

// migrationBackend is the backend the upgrade test seeds.
const migrationBackend = "migrated"

// TestMigrations_DeriveOrphanBytesFromCleanupRows verifies the upgrade drops
// the stale counter in favour of the sum of the backend's queue and DLQ rows,
// adds the part path column, and that a rollback writes that sum back into the
// counter.
func TestMigrations_DeriveOrphanBytesFromCleanupRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := freshDatabase(ctx, t)
	migrateTo(ctx, t, s.connStr, counterSchemaVersion)

	for _, stmt := range []string{
		`INSERT INTO backend_quotas (backend_name, bytes_limit, orphan_bytes) VALUES ('` + migrationBackend + `', 1000, 999)`,
		`INSERT INTO cleanup_queue (backend_name, object_key, storage_key, reason, size_bytes) VALUES ('` + migrationBackend + `', 'k', 'k', 'r', 100), ('` + migrationBackend + `', 'k', 'k2', 'r', 50)`,
		`INSERT INTO cleanup_dlq (original_id, backend_name, object_key, storage_key, reason, size_bytes, attempts, first_enqueued_at) VALUES (1, '` + migrationBackend + `', 'k', 'k3', 'r', 25, 10, NOW())`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	migrateTo(ctx, t, s.connStr, ExpectedSchemaVersion)
	var derived int64
	if err := s.pool.QueryRow(ctx, `SELECT orphan_bytes FROM backend_capacity WHERE backend_name = $1`, migrationBackend).Scan(&derived); err != nil {
		t.Fatalf("read backend_capacity: %v", err)
	}
	if derived != 175 {
		t.Errorf("derived orphan_bytes = %d, want 175 from the queue and DLQ rows", derived)
	}
	if _, err := s.pool.Exec(ctx, `SELECT storage_key FROM multipart_parts LIMIT 0`); err != nil {
		t.Errorf("multipart_parts.storage_key missing after the upgrade: %v", err)
	}

	migrateDownTo(ctx, t, s.connStr, counterSchemaVersion)
	var counter int64
	if err := s.pool.QueryRow(ctx, `SELECT orphan_bytes FROM backend_quotas WHERE backend_name = $1`, migrationBackend).Scan(&counter); err != nil {
		t.Fatalf("read restored counter: %v", err)
	}
	if counter != 175 {
		t.Errorf("restored orphan_bytes = %d, want 175", counter)
	}
}

// freshDatabase creates an empty database on the shared container and
// returns a store connected to it, dropping the database when the test ends.
func freshDatabase(ctx context.Context, t *testing.T) *Store {
	t.Helper()
	shared := adapterPgStore(t)
	dbName := fmt.Sprintf("migrate_orphans_%d", time.Now().UnixNano())
	if _, err := shared.pool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = shared.pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})
	conn := shared.pool.Config().ConnConfig
	s, err := NewStore(ctx, &config.DatabaseConfig{
		Host: conn.Host, Port: int(conn.Port), Database: dbName, User: conn.User,
		Password: conn.Password, SSLMode: "disable", MaxConns: 2, MinConns: 1,
	}, nil, pgFixtureInstance)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// migrateDownTo rolls the embedded migrations back to version.
func migrateDownTo(ctx context.Context, t *testing.T, connStr string, version int64) {
	t.Helper()
	stdDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open migration connection: %v", err)
	}
	defer stdDB.Close()
	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("migration filesystem: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, stdDB, migrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		t.Fatalf("migrate down to %d: %v", version, err)
	}
}
