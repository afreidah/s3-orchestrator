// -------------------------------------------------------------------------------
// SQLite Migrations - Schema Initialization and Version Management
//
// Author: Alex Freidah
//
// Manages the SQLite schema lifecycle. Embeds the consolidated schema DDL via
// go:embed and applies it on first run. Subsequent starts verify the schema
// version matches the expected version.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
)

//go:embed schema.sql
var schemaSQL string

// expectedSchemaVersion is the SQLite schema version this binary expects.
// Bump this when the embedded schema.sql is updated.
const expectedSchemaVersion = 1

// RunMigrations applies the embedded SQLite schema if the database has not
// been initialised yet. If schema_version already exists and the version
// matches, this is a no-op. If the version does not match, an error is
// returned so the operator can take corrective action.
func (s *Store) RunMigrations(ctx context.Context) error {
	version, exists, err := s.currentSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	if exists {
		if version == expectedSchemaVersion {
			slog.InfoContext(ctx, "SQLite schema up to date", "version", version)
			return nil
		}
		return fmt.Errorf(
			"SQLite schema version %d does not match expected %d — manual migration required",
			version, expectedSchemaVersion,
		)
	}

	// Fresh database: apply the full schema inside a transaction.
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply sqlite schema: %w", err)
	}

	slog.InfoContext(ctx, "SQLite schema applied", "version", expectedSchemaVersion)
	return nil
}

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
		return fmt.Errorf("schema_version table does not exist — database not initialised")
	}

	if version < expectedSchemaVersion {
		return fmt.Errorf(
			"SQLite schema version %d is older than expected %d — migrations may have partially failed",
			version, expectedSchemaVersion,
		)
	}
	if version > expectedSchemaVersion {
		slog.WarnContext(ctx, "SQLite schema is newer than this binary expects — possible downgrade",
			"schema_version", version, "expected", expectedSchemaVersion,
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
