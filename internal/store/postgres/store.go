// -------------------------------------------------------------------------------
// Store - Core Types, Constructor, and Helpers
//
// Author: Alex Freidah
//
// The PostgreSQL Store: the pool and its circuit breaker, migrations and the
// schema check, the transaction helpers core's shared methods run through, and
// the row converters every query file maps sqlc rows with.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	// Registers the pgx database/sql driver used by goose migrations below.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Store manages quota and object location data in PostgreSQL. The embedded
// core.TxOps supplies the methods whose whole body is a core transaction over
// this store as Runner; the methods declared here are the postgres-specific
// queries.
type Store struct {
	core.TxOps

	pool    *pgxpool.Pool
	queries *db.Queries
	cb      *breaker.CircuitBreaker
	connStr string

	instanceID string
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewStore creates a new PostgreSQL store connection using pgxpool.
// When cb is non-nil, every sqlc query (pool-bound or tx-bound) flows
// through it via wrapDBTX. Pass nil to skip CB wrapping (test fixtures,
// migration runners). instanceID claims the cleanup rows this process's
// transactions queue, so the cleanup worker leaves them to it until the claim
// grace period passes.
func NewStore(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker, instanceID string) (*Store, error) {
	host := net.JoinHostPort(dbCfg.Host, strconv.Itoa(dbCfg.Port))
	connStr := dbCfg.ConnectionString()
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse db connection string (host=%s db=%s): %w", host, dbCfg.Database, err)
	}

	cfg.MaxConns = dbCfg.MaxConns
	cfg.MinConns = dbCfg.MinConns
	// A zero lifetime means "unset": keep pgxpool's default rather than
	// overriding it with 0. Since pgx v5.10.0 the pool expires connections at
	// acquire time (createdAt + MaxConnLifetime), so a 0 lifetime expires every
	// connection instantly and Acquire fails with "too many failed attempts".
	if dbCfg.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = dbCfg.MaxConnLifetime
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create db connection pool (host=%s db=%s): %w", host, dbCfg.Database, err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf(
			"connect to database (host=%s db=%s): %w; verify the host is reachable, the port matches the server, the database exists, and the credentials are valid",
			host, dbCfg.Database, err)
	}

	s := &Store{
		pool:       pool,
		queries:    db.New(wrapDBTX(pool, cb)),
		cb:         cb,
		connStr:    connStr,
		instanceID: instanceID,
	}
	s.TxOps = core.NewTxOps(s)
	return s, nil
}

// Close closes the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping checks the database answers, outside the circuit breaker, so the
// breaker's recovery can find out the database is back while it refuses
// every query.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// RunMigrations applies the embedded goose migrations. Instances starting
// together take turns under an advisory lock: the first applies the
// migrations, and the rest wait, then find nothing left to apply.
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

	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(core.LockMigrations))
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, stdDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		slog.InfoContext(ctx, "migration applied",
			logfmt.Component("postgres_store"),
			"version", r.Source.Version,
			"duration", r.Duration)
	}
	return nil
}

// ExpectedSchemaVersion is the migration version this binary expects.
// Updated when new migration files are added.
const ExpectedSchemaVersion = 48

// VerifySchemaVersion checks that the database schema version matches what
// this binary expects. Returns an error if the schema is older (a partial
// migration) or newer (an outdated binary) than expected.
func (s *Store) VerifySchemaVersion(ctx context.Context) error {
	var version int
	err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = true",
	).Scan(&version)
	if err != nil {
		return fmt.Errorf("query schema version: %w", err)
	}
	return core.CheckSchemaVersion("database", version, ExpectedSchemaVersion)
}

// -------------------------------------------------------------------------
// TRANSACTION HELPERS
// -------------------------------------------------------------------------

// withTx executes fn within a transaction, committing on success or rolling
// back on error. The tx-bound Queries handle threads through wrapDBTX so
// the breaker observes statement failures inside the transaction.
func (s *Store) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(db.New(wrapDBTX(tx, s.cb))); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// WithTx satisfies core.Runner by running fn in a transaction over a
// pgTxAdapter, so engine-agnostic core helpers can orchestrate multi-statement
// operations against the postgres engine.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx core.TxAdapter) error) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		return fn(ctx, &pgTxAdapter{q: q, instanceID: s.instanceID})
	})
}

// slimObjectRow is the minimum surface of sqlc rows that project an
// ObjectLocation without encryption columns. Implemented by the four list
// queries that pull only key/backend/size/created_at.
type slimObjectRow interface {
	GetObjectKey() string
	GetBackendName() string
	GetStorageKey() string
	GetSizeBytes() int64
	GetCreatedAt() pgtype.Timestamptz
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
			StorageKey:  r.GetStorageKey(),
			SizeBytes:   r.GetSizeBytes(),
			CreatedAt:   r.GetCreatedAt().Time,
		}
	}
	return out
}

// toObjectLocation converts a whole object_locations row, the table model every
// SELECT * query returns. A NULL column leaves its zero value, and a metadata
// decode failure leaves the identity nil, costing a backend round trip rather
// than the read.
func toObjectLocation(r *db.ObjectLocation) core.ObjectLocation {
	loc := core.ObjectLocation{
		Encrypted:                r.Encrypted,
		EncryptionKey:            r.EncryptionKey,
		KeyID:                    derefStr(r.KeyID),
		PlaintextSize:            derefInt64(r.PlaintextSize),
		ContentHash:              derefStr(r.ContentHash),
		CompressionAlgorithm:     derefStr(r.CompressionAlgorithm),
		CompressionLevel:         derefStr(r.CompressionLevel),
		CompressionFormatVersion: int(derefInt16(r.CompressionFormatVersion)),
		LogicalSize:              derefInt64(r.LogicalSize),
		ObjectKey:                r.ObjectKey,
		BackendName:              r.BackendName,
		StorageKey:               r.StorageKey,
		SizeBytes:                r.SizeBytes,
		CreatedAt:                r.CreatedAt.Time,
		CompressionProbeSize:     derefInt64(r.CompressionProbeSize),
		CompressionProbeLevel:    derefStr(r.CompressionProbeLevel),
		Unmanaged:                !r.Managed,
	}
	if r.LastScrubbedAt.Valid {
		loc.LastScrubbedAt = new(r.LastScrubbedAt.Time)
	}
	if id, err := core.IdentityFromColumns(derefStr(r.Etag), derefStr(r.ContentType), r.UserMetadata); err == nil {
		loc.Identity = id
	}
	return loc
}

// toObjectLocations converts the whole object_locations rows a list query
// returns, one toObjectLocation each.
func toObjectLocations(rows []db.ObjectLocation) []core.ObjectLocation {
	out := make([]core.ObjectLocation, len(rows))
	for i := range rows {
		out[i] = toObjectLocation(&rows[i])
	}
	return out
}

// pgTimestamptz converts a time.Time to pgtype.Timestamptz for use with sqlc.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
