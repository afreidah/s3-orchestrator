// -------------------------------------------------------------------------------
// SQLite Migrations - Schema Initialization and Version Management
//
// Author: Alex Freidah
//
// Manages the SQLite schema lifecycle. Embeds the consolidated schema DDL and
// applies it on first run. Subsequent starts verify the schema version matches
// the expected version.
// -------------------------------------------------------------------------------

package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// EMBEDDED SCHEMA
// -------------------------------------------------------------------------

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/*.sql
var migrationFS embed.FS

// expectedSchemaVersion is the SQLite schema version this binary expects. Bump
// it when adding a migration, and keep schema.sql's own INSERT INTO
// schema_version in step so a fresh database and an upgraded one agree.
const expectedSchemaVersion = 14

// migration is one numbered step, parsed from its embedded file name.
type migration struct {
	version int
	name    string
	sql     string
}

// migrationDir is the directory the migrations are embedded under.
const migrationDir = "migrations"

// -------------------------------------------------------------------------
// MIGRATION LOADING
// -------------------------------------------------------------------------

// loadMigrations reads the embedded migrations in ascending version order.
func loadMigrations() ([]migration, error) {
	return loadMigrationsFrom(migrationFS)
}

// loadMigrationsFrom reads migrations from fsys in ascending version order. The
// file name carries the version, so ordering is explicit in the tree rather
// than implied by a registration list someone has to remember to update.
//
// The filesystem is a parameter so the naming rules this enforces can be
// exercised against deliberately malformed trees. A runner that silently
// mis-orders or skips a migration is worse than one that refuses to start.
func loadMigrationsFrom(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("read sqlite migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, rest, found := strings.Cut(e.Name(), "_")
		if !found {
			return nil, fmt.Errorf("migration %q is not named <version>_<description>.sql", e.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version prefix: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(fsys, migrationDir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: version,
			name:    strings.TrimSuffix(rest, ".sql"),
			sql:     string(body),
		})
	}

	slices.SortFunc(out, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return out, nil
}

// -------------------------------------------------------------------------
// APPLYING
// -------------------------------------------------------------------------

// RunMigrations brings the database up to expectedSchemaVersion.
//
// A fresh database gets schema.sql, which establishes the baseline and records
// its own version. An existing database has every numbered migration above its
// recorded version applied in order. Both then land on the same version, so a
// database created today and one upgraded through several releases are
// identical.
//
// Each migration runs in its own transaction with its version row written in
// the same commit: a migration either applied and is recorded, or did neither.
// A version ahead of this binary is still an error, since a downgrade cannot
// know what a later release changed.
func (s *Store) RunMigrations(ctx context.Context) error {
	version, exists, err := s.currentSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	if !exists {
		if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply sqlite schema: %w", err)
		}
		if version, _, err = s.currentSchemaVersion(ctx); err != nil {
			return fmt.Errorf("read baseline schema version: %w", err)
		}
		slog.InfoContext(ctx, "SQLite schema applied",
			logfmt.Component("sqlite_store"),
			"version", version,
		)
	}

	if version > expectedSchemaVersion {
		return fmt.Errorf(
			"SQLite schema version %d is newer than expected %d  -  binary is outdated",
			version, expectedSchemaVersion,
		)
	}

	applied, err := s.applyPendingMigrations(ctx, version)
	if err != nil {
		return err
	}

	if applied == 0 {
		slog.InfoContext(ctx, "SQLite schema up to date",
			logfmt.Component("sqlite_store"),
			"version", expectedSchemaVersion,
		)
	}
	return nil
}

// applyPendingMigrations runs every embedded migration numbered above from, in
// ascending order, and reports how many ran.
func (s *Store) applyPendingMigrations(ctx context.Context, from int) (int, error) {
	pending, err := loadMigrations()
	if err != nil {
		return 0, err
	}

	var applied int
	for _, m := range pending {
		if m.version <= from {
			continue
		}
		if m.version > expectedSchemaVersion {
			return applied, fmt.Errorf(
				"migration %d is newer than the version this binary expects (%d)",
				m.version, expectedSchemaVersion,
			)
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return applied, err
		}
		applied++
		slog.InfoContext(ctx, "SQLite migration applied",
			logfmt.Component("sqlite_store"),
			"version", m.version,
			"name", m.name,
		)
	}
	return applied, nil
}

// applyMigration runs one migration and records its version in the same
// transaction, so a partially applied migration cannot be recorded as done.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version (version) VALUES (?)", m.version); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		return nil
	})
}

// -------------------------------------------------------------------------
// VERIFICATION
// -------------------------------------------------------------------------

// VerifySchemaVersion checks that the database schema version matches what
// this binary expects. Returns an error if schema_version is missing or if
// the recorded version is older than expected. Logs a warning if the schema
// is newer (possible downgrade).
func (s *Store) VerifySchemaVersion(ctx context.Context) error {
	version, exists, err := s.currentSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("query schema version: %w", err)
	}
	if !exists {
		return fmt.Errorf("schema_version table does not exist  -  database not initialised")
	}

	if version < expectedSchemaVersion {
		return fmt.Errorf(
			"SQLite schema version %d is older than expected %d  -  migrations may have partially failed",
			version, expectedSchemaVersion,
		)
	}
	if version > expectedSchemaVersion {
		return fmt.Errorf(
			"SQLite schema version %d is newer than expected %d  -  binary is outdated",
			version, expectedSchemaVersion,
		)
	}
	return nil
}

// currentSchemaVersion returns the version from the schema_version table.
// If the table does not exist, exists is false and version is 0.
func (s *Store) currentSchemaVersion(ctx context.Context) (version int, exists bool, err error) {
	// Check whether the schema_version table exists.
	var count int
	err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_version'",
	).Scan(&count)
	if err != nil {
		return 0, false, fmt.Errorf("check sqlite_master: %w", err)
	}
	if count == 0 {
		return 0, false, nil
	}

	err = s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_version",
	).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return 0, true, fmt.Errorf("read schema_version: %w", err)
	}
	return version, true, nil
}
