// -------------------------------------------------------------------------------
// Store - Core Types, Constructor, and Helpers
//
// Author: Alex Freidah
//
// Manages quota tracking and object location storage in PostgreSQL. Tracks which
// backend stores each object and how much quota each backend has used. Provides
// atomic operations to ensure quota limits are respected.
// -------------------------------------------------------------------------------

// Package store provides PostgreSQL metadata persistence for the S3 orchestrator.
// metadata tracking, quota enforcement, circuit breaker protection, replication,
// and rebalancing.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the pgx database/sql driver used by goose migrations below.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// likeEscaper escapes SQL LIKE wildcards in prefix strings.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Store manages quota and object location data in PostgreSQL.
type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	connStr string
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewStore creates a new PostgreSQL store connection using pgxpool.
func NewStore(ctx context.Context, dbCfg *config.DatabaseConfig) (*Store, error) {
	connStr := dbCfg.ConnectionString()
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	cfg.MaxConns = dbCfg.MaxConns
	cfg.MinConns = dbCfg.MinConns
	cfg.MaxConnLifetime = dbCfg.MaxConnLifetime
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return &Store{
		pool:    pool,
		queries: db.New(pool),
		connStr: connStr,
	}, nil
}

// Close closes the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// RunMigrations applies versioned database migrations using goose. Migrations
// are embedded in the binary and applied in order. Already-applied migrations
// are skipped automatically via the goose_db_version tracking table.
func (s *Store) RunMigrations(ctx context.Context) error {
	stdDB, err := sql.Open("pgx", s.connStr)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer stdDB.Close()

	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("migration filesystem: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, stdDB, migrations)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		slog.InfoContext(ctx, "migration applied",
			"version", r.Source.Version,
			"duration", r.Duration)
	}
	return nil
}

// ExpectedSchemaVersion is the migration version this binary expects.
// Updated when new migration files are added.
const ExpectedSchemaVersion = 10

// VerifySchemaVersion checks that the database schema version matches
// what this binary expects. Returns an error if the schema is older
// than expected (partial migration failure). Logs a warning if the
// schema is newer (possible downgrade).
func (s *Store) VerifySchemaVersion(ctx context.Context) error {
	var version int64
	err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = true",
	).Scan(&version)
	if err != nil {
		return fmt.Errorf("query schema version: %w", err)
	}

	if version < ExpectedSchemaVersion {
		return fmt.Errorf("database schema version %d is older than expected %d  -  migrations may have partially failed", version, ExpectedSchemaVersion)
	}
	if version > ExpectedSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than expected %d  -  binary is outdated", version, ExpectedSchemaVersion)
	}
	return nil
}

// -------------------------------------------------------------------------
// TRANSACTION HELPERS
// -------------------------------------------------------------------------

// withTx executes fn within a transaction, committing on success or rolling
// back on error.
func (s *Store) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(s.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithTx satisfies core.Runner by opening a transaction, wrapping the
// sqlc Queries in a pgTxAdapter, and invoking fn. Commits on a nil
// return; rolls back otherwise. Lets engine-agnostic core helpers
// orchestrate multi-statement operations against the postgres engine.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx core.TxAdapter) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	adapter := &pgTxAdapter{q: s.queries.WithTx(tx)}
	if err := fn(ctx, adapter); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Compile-time check that *Store satisfies core.Runner.
var _ core.Runner = (*Store)(nil)

// slimObjectRow is the minimum surface of sqlc rows that project an
// ObjectLocation without encryption columns. Implemented by the four list
// queries that pull only key/backend/size/created_at.
type slimObjectRow interface {
	GetObjectKey() string
	GetBackendName() string
	GetSizeBytes() int64
	GetCreatedAt() pgtype.Timestamptz
}

// fatObjectRow extends slimObjectRow with the encryption + content-hash
// columns the seven encryption-aware queries return.
type fatObjectRow interface {
	slimObjectRow
	GetEncrypted() bool
	GetEncryptionKey() []byte
	GetKeyID() *string
	GetPlaintextSize() *int64
	GetContentHash() *string
}

// toSlimObjectLocations converts a slice of slim sqlc rows. Encryption and
// content-hash fields stay zero-valued.
func toSlimObjectLocations[T slimObjectRow](rows []T) []core.ObjectLocation {
	out := make([]core.ObjectLocation, len(rows))
	for i := range rows {
		r := rows[i]
		out[i] = core.ObjectLocation{
			ObjectKey:   r.GetObjectKey(),
			BackendName: r.GetBackendName(),
			SizeBytes:   r.GetSizeBytes(),
			CreatedAt:   r.GetCreatedAt().Time,
		}
	}
	return out
}

// toFatObjectLocations converts a slice of encryption-aware sqlc rows.
// Pointer-typed nullable columns are safely dereferenced.
func toFatObjectLocations[T fatObjectRow](rows []T) []core.ObjectLocation {
	out := make([]core.ObjectLocation, len(rows))
	for i := range rows {
		r := rows[i]
		loc := core.ObjectLocation{
			ObjectKey:     r.GetObjectKey(),
			BackendName:   r.GetBackendName(),
			SizeBytes:     r.GetSizeBytes(),
			CreatedAt:     r.GetCreatedAt().Time,
			Encrypted:     r.GetEncrypted(),
			EncryptionKey: r.GetEncryptionKey(),
		}
		if k := r.GetKeyID(); k != nil {
			loc.KeyID = *k
		}
		if p := r.GetPlaintextSize(); p != nil {
			loc.PlaintextSize = *p
		}
		if h := r.GetContentHash(); h != nil {
			loc.ContentHash = *h
		}
		out[i] = loc
	}
	return out
}

// pgTimestamptz converts a time.Time to pgtype.Timestamptz for use with sqlc.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
