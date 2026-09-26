// -------------------------------------------------------------------------------
// Postgres Concurrent Migration Integration Test
//
// Author: Alex Freidah
//
// Several instances of a new release start together against a database that
// has migrations pending, and all run them at once. Without the migration
// lock, two of them apply the same migration and the loser fails on a column
// that already exists.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// migratingInstances is how many stores race to migrate the same database.
const migratingInstances = 5

// priorSchemaVersion is the version the database is at before the race, so
// the instances contend over every migration after it.
const priorSchemaVersion = 10

// TestRunMigrations_ConcurrentInstances verifies instances migrating one
// database at the same time all succeed and leave it at the expected version.
func TestRunMigrations_ConcurrentInstances(t *testing.T) {
	shared := adapterPgStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	dbName := fmt.Sprintf("migrate_race_%d", time.Now().UnixNano())
	if _, err := shared.pool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = shared.pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	conn := shared.pool.Config().ConnConfig
	stores := make([]*Store, migratingInstances)
	for i := range stores {
		s, err := NewStore(ctx, &config.DatabaseConfig{
			Host:     conn.Host,
			Port:     int(conn.Port),
			Database: dbName,
			User:     conn.User,
			Password: conn.Password,
			SSLMode:  "disable",
			MaxConns: 2,
			MinConns: 1,
		}, nil)
		if err != nil {
			t.Fatalf("NewStore %d: %v", i, err)
		}
		t.Cleanup(s.Close)
		stores[i] = s
	}
	migrateTo(ctx, t, stores[0].connStr, priorSchemaVersion)

	start := make(chan struct{})
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Go(func() {
			<-start
			errs[i] = s.RunMigrations(ctx)
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d: RunMigrations: %v", i, err)
		}
	}
	if err := stores[0].VerifySchemaVersion(ctx); err != nil {
		t.Errorf("VerifySchemaVersion after concurrent migrations: %v", err)
	}
}

// migrateTo applies the embedded migrations up to version, standing in for a
// database left behind by an earlier release.
func migrateTo(ctx context.Context, t *testing.T, connStr string, version int64) {
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
	if _, err := provider.UpTo(ctx, version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}
