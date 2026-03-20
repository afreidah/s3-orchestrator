// -------------------------------------------------------------------------------
// Store - PostgreSQL Quota and Object Location Storage
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
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/afreidah/s3-orchestrator/internal/config"
	db "github.com/afreidah/s3-orchestrator/internal/store/sqlc"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// likeEscaper escapes SQL LIKE wildcards in prefix strings.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// -------------------------------------------------------------------------
// ERRORS
// -------------------------------------------------------------------------

// S3Error is a structured error that carries an HTTP status code and S3 error
// code, allowing the server layer to translate storage errors into S3 XML
// responses without per-handler error mapping.
type S3Error struct {
	StatusCode int    // HTTP status code (e.g. 404, 507)
	Code       string // S3 error code (e.g. "NoSuchKey")
	Message    string // Human-readable message
}

// Error returns the human-readable error message.
func (e *S3Error) Error() string {
	return e.Message
}

var (
	// ErrNoSpaceAvailable is an internal error used between store and manager.
	ErrNoSpaceAvailable = errors.New("no backend has sufficient quota")

	// ErrObjectNotFound is returned when an object is not in the location table.
	ErrObjectNotFound = &S3Error{StatusCode: 404, Code: "NoSuchKey", Message: "object not found"}

	// ErrMultipartUploadNotFound is returned when a multipart upload ID is not found.
	ErrMultipartUploadNotFound = &S3Error{StatusCode: 404, Code: "NoSuchUpload", Message: "multipart upload not found"}
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Store manages quota and object location data in PostgreSQL.
type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	connStr string
}

// QuotaStat holds quota statistics for a single backend.
type QuotaStat struct {
	BackendName string
	BytesUsed   int64
	BytesLimit  int64
	OrphanBytes int64
	UpdatedAt   time.Time
}

// DeletedCopy holds information about a single deleted copy of an object.
type DeletedCopy struct {
	BackendName string
	SizeBytes   int64
}

// ObjectLocation holds information about where an object is stored, including
// optional encryption metadata for objects encrypted with envelope encryption.
type ObjectLocation struct {
	ObjectKey     string
	BackendName   string
	SizeBytes     int64
	CreatedAt     time.Time
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
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
func (s *Store) RunMigrations(ctx context.Context) error { // codecov:ignore -- requires live PostgreSQL, covered by integration tests
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

// withTxVal executes fn within a transaction and returns its result,
// committing on success or rolling back on error.
func withTxVal[T any](s *Store, ctx context.Context, fn func(*db.Queries) (T, error)) (T, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	val, err := fn(s.queries.WithTx(tx))
	if err != nil {
		var zero T
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		var zero T
		return zero, fmt.Errorf("failed to commit: %w", err)
	}
	return val, nil
}

// objectLocationRow is a type constraint matching all sqlc row types that
// carry the core object location columns.
type objectLocationRow interface {
	db.ListObjectsByBackendRow |
		db.ListObjectsByPrefixRow |
		db.ListExpiredObjectsRow |
		db.ListDirectChildrenRow |
		db.GetAllObjectLocationsRow |
		db.GetUnderReplicatedObjectsRow |
		db.GetUnderReplicatedObjectsExcludingRow |
		db.GetOverReplicatedObjectsRow |
		db.GetObjectCopiesForUpdateRow
}

// toObjectLocations converts any sqlc row type containing object location
// columns into storage ObjectLocations via the common conversion helper.
func toObjectLocations[T objectLocationRow](rows []T) []ObjectLocation {
	out := make([]ObjectLocation, len(rows))
	for i := range rows {
		out[i] = toObjectLocation(any(rows[i]))
	}
	return out
}

// toObjectLocation converts a single sqlc row (passed as any) into an
// ObjectLocation. Row types that include encryption columns populate those
// fields; simpler row types leave them at zero values.
func toObjectLocation(row any) ObjectLocation {
	switch r := row.(type) {
	case db.GetAllObjectLocationsRow:
		return objectLocationFromDB(r.ObjectKey, r.BackendName, r.SizeBytes,
			r.Encrypted, r.EncryptionKey, r.KeyID, r.PlaintextSize, r.CreatedAt.Time)
	case db.GetUnderReplicatedObjectsRow:
		return objectLocationFromDB(r.ObjectKey, r.BackendName, r.SizeBytes,
			r.Encrypted, r.EncryptionKey, r.KeyID, r.PlaintextSize, r.CreatedAt.Time)
	case db.GetUnderReplicatedObjectsExcludingRow:
		return objectLocationFromDB(r.ObjectKey, r.BackendName, r.SizeBytes,
			r.Encrypted, r.EncryptionKey, r.KeyID, r.PlaintextSize, r.CreatedAt.Time)
	case db.GetOverReplicatedObjectsRow:
		return objectLocationFromDB(r.ObjectKey, r.BackendName, r.SizeBytes,
			r.Encrypted, r.EncryptionKey, r.KeyID, r.PlaintextSize, r.CreatedAt.Time)
	case db.GetObjectCopiesForUpdateRow:
		return objectLocationFromDB(r.ObjectKey, r.BackendName, r.SizeBytes,
			r.Encrypted, r.EncryptionKey, r.KeyID, r.PlaintextSize, r.CreatedAt.Time)
	case db.ListObjectsByBackendRow:
		return ObjectLocation{ObjectKey: r.ObjectKey, BackendName: r.BackendName, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time}
	case db.ListObjectsByPrefixRow:
		return ObjectLocation{ObjectKey: r.ObjectKey, BackendName: r.BackendName, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time}
	case db.ListExpiredObjectsRow:
		return ObjectLocation{ObjectKey: r.ObjectKey, BackendName: r.BackendName, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time}
	case db.ListDirectChildrenRow:
		return ObjectLocation{ObjectKey: r.ObjectKey, BackendName: r.BackendName, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time}
	default:
		return ObjectLocation{}
	}
}

// objectLocationFromDB builds an ObjectLocation from database column values,
// safely dereferencing nullable pointer fields.
func objectLocationFromDB(key, backend string, size int64, encrypted bool, encKey []byte, keyID *string, ptSize *int64, created time.Time) ObjectLocation {
	loc := ObjectLocation{
		ObjectKey:     key,
		BackendName:   backend,
		SizeBytes:     size,
		CreatedAt:     created,
		Encrypted:     encrypted,
		EncryptionKey: encKey,
	}
	if keyID != nil {
		loc.KeyID = *keyID
	}
	if ptSize != nil {
		loc.PlaintextSize = *ptSize
	}
	return loc
}

// -------------------------------------------------------------------------
// QUOTA OPERATIONS
// -------------------------------------------------------------------------

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
// All updates happen in a single transaction for atomicity.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error {
	return s.withTx(ctx, func(qtx *db.Queries) error {
		for i := range backends {
			err := qtx.UpsertQuotaLimit(ctx, db.UpsertQuotaLimitParams{
				BackendName: backends[i].Name,
				BytesLimit:  backends[i].QuotaBytes,
			})
			if err != nil {
				return fmt.Errorf("failed to sync quota for backend %s: %w", backends[i].Name, err)
			}
		}
		return nil
	})
}

// GetBackendWithSpace finds a backend with enough quota for the given size.
// Returns the backend name or ErrNoSpaceAvailable if none have enough space.
func (s *Store) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	for _, name := range backendOrder {
		available, err := s.queries.GetBackendAvailableSpace(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to check quota for %s: %w", name, err)
		}

		if available >= size {
			return name, nil
		}
	}

	return "", ErrNoSpaceAvailable
}

// GetLeastUtilizedBackend finds the backend with the lowest utilization ratio
// that has enough space for the given size. Used by the "spread" routing strategy.
func (s *Store) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	row, err := s.queries.GetLeastUtilizedBackend(ctx, db.GetLeastUtilizedBackendParams{
		BackendNames: eligible,
		MinSize:      size,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSpaceAvailable
	}
	if err != nil {
		return "", fmt.Errorf("failed to find least utilized backend: %w", err)
	}
	return row.BackendName, nil
}

// GetQuotaStats returns quota statistics for all backends.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error) {
	rows, err := s.queries.GetAllQuotaStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}

	stats := make(map[string]QuotaStat)
	for _, row := range rows {
		stats[row.BackendName] = QuotaStat{
			BackendName: row.BackendName,
			BytesUsed:   row.BytesUsed,
			BytesLimit:  row.BytesLimit,
			OrphanBytes: row.OrphanBytes,
			UpdatedAt:   row.UpdatedAt.Time,
		}
	}

	return stats, nil
}

// GetObjectCounts returns the number of objects stored on each backend.
func (s *Store) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetObjectCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query object counts: %w", err)
	}

	counts := make(map[string]int64)
	for _, row := range rows {
		counts[row.BackendName] = row.ObjectCount
	}
	return counts, nil
}

// GetActiveMultipartCounts returns the number of in-progress multipart uploads
// per backend.
func (s *Store) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetActiveMultipartCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query multipart counts: %w", err)
	}

	counts := make(map[string]int64)
	for _, row := range rows {
		counts[row.BackendName] = row.UploadCount
	}
	return counts, nil
}

// -------------------------------------------------------------------------
// OBJECT LOCATION OPERATIONS
// -------------------------------------------------------------------------

// RecordObject records an object's location and updates the backend quota.
// On overwrite, all existing copies (including replicas) are removed and their
// quotas decremented before inserting the new primary copy.
// EncryptionMeta holds encryption metadata to store alongside an object
// location. Zero value represents an unencrypted object.
type EncryptionMeta struct {
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// RecordObject atomically inserts or updates an object location, handling
// overwrites by returning displaced copies for cleanup.
func (s *Store) RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) ([]DeletedCopy, error) {
		// Serialize concurrent writes for the same key to prevent orphans
		if err := qtx.LockObjectKeyForWrite(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to acquire object key lock: %w", err)
		}

		// --- Collect all existing copies for this key ---
		existing, err := qtx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to query existing copies: %w", err)
		}

		// --- Delete all existing copies and decrement their quotas ---
		// Collect displaced copies on OTHER backends for orphan cleanup.
		var displaced []DeletedCopy
		if len(existing) > 0 {
			if err := qtx.DeleteObjectCopies(ctx, key); err != nil {
				return nil, fmt.Errorf("failed to delete existing copies: %w", err)
			}

			for _, ec := range existing {
				if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
					Amount:      ec.SizeBytes,
					BackendName: ec.BackendName,
				}); err != nil {
					return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.BackendName, err)
				}
				// The new write goes to `backend`. Copies on OTHER backends
				// need physical cleanup — the new PutObject overwrites
				// in-place on the target backend, but stale copies on other
				// backends become orphans.
				if ec.BackendName != backend {
					displaced = append(displaced, DeletedCopy{
						BackendName: ec.BackendName,
						SizeBytes:   ec.SizeBytes,
					})
				}
			}
		}

		// --- Build insert params with optional encryption metadata ---
		params := db.InsertObjectLocationParams{
			ObjectKey:   key,
			BackendName: backend,
			SizeBytes:   size,
		}
		if enc != nil && enc.Encrypted {
			params.Encrypted = true
			params.EncryptionKey = enc.EncryptionKey
			params.KeyID = &enc.KeyID
			params.PlaintextSize = &enc.PlaintextSize
		}

		// --- Insert new primary copy ---
		if err := qtx.InsertObjectLocation(ctx, params); err != nil {
			return nil, fmt.Errorf("failed to insert object location: %w", err)
		}

		// --- Increment quota for new backend ---
		n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
			Amount:      size,
			BackendName: backend,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to update quota: %w", err)
		}
		if n == 0 {
			return nil, ErrNoSpaceAvailable
		}

		return displaced, nil
	})
}

// DeleteObject removes all copies of an object and decrements their quotas.
// Returns all deleted copies, or ErrObjectNotFound if the object doesn't exist.
func (s *Store) DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) ([]DeletedCopy, error) {
		// --- Get all copies ---
		existing, err := qtx.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to get object locations: %w", err)
		}

		if len(existing) == 0 {
			return nil, ErrObjectNotFound
		}

		// --- Delete all location records ---
		if err := qtx.DeleteObjectCopies(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to delete object locations: %w", err)
		}

		// --- Decrement quota for each backend ---
		copies := make([]DeletedCopy, len(existing))
		for i, ec := range existing {
			copies[i] = DeletedCopy{
				BackendName: ec.BackendName,
				SizeBytes:   ec.SizeBytes,
			}
			if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
				Amount:      ec.SizeBytes,
				BackendName: ec.BackendName,
			}); err != nil {
				return nil, fmt.Errorf("failed to decrement quota for %s: %w", ec.BackendName, err)
			}
		}

		return copies, nil
	})
}

// ListObjectsByBackend returns objects stored on a specific backend, ordered by
// size ascending (smallest first). Used by the rebalancer to find movable objects.
func (s *Store) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error) {
	rows, err := s.queries.ListObjectsByBackend(ctx, db.ListObjectsByBackendParams{
		BackendName: backendName,
		Limit:       int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects by backend: %w", err)
	}
	return toObjectLocations(rows), nil
}

// MoveObjectLocation atomically moves a copy of an object from one backend to
// another. Uses SELECT FOR UPDATE to prevent races. Returns (0, nil) if the
// source copy is gone or the target already has a copy.
func (s *Store) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) (int64, error) {
		// --- Check if target backend already has a copy ---
		exists, err := qtx.CheckObjectExistsOnBackend(ctx, db.CheckObjectExistsOnBackendParams{
			ObjectKey:   key,
			BackendName: toBackend,
		})
		if err != nil {
			return 0, fmt.Errorf("failed to check target: %w", err)
		}
		if exists {
			return 0, nil
		}

		// --- Lock the source row and verify it still belongs to the source ---
		locked, err := qtx.LockObjectOnBackend(ctx, db.LockObjectOnBackendParams{
			ObjectKey:   key,
			BackendName: fromBackend,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("failed to lock object: %w", err)
		}

		// --- Delete source row ---
		if err := qtx.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
			ObjectKey:   key,
			BackendName: fromBackend,
		}); err != nil {
			return 0, fmt.Errorf("failed to delete source location: %w", err)
		}

		// --- Insert destination row preserving encryption metadata ---
		if err := qtx.InsertObjectLocation(ctx, db.InsertObjectLocationParams{
			ObjectKey:     key,
			BackendName:   toBackend,
			SizeBytes:     locked.SizeBytes,
			Encrypted:     locked.Encrypted,
			EncryptionKey: locked.EncryptionKey,
			KeyID:         locked.KeyID,
			PlaintextSize: locked.PlaintextSize,
		}); err != nil {
			return 0, fmt.Errorf("failed to insert destination location: %w", err)
		}

		// --- Decrement source quota ---
		if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
			Amount:      locked.SizeBytes,
			BackendName: fromBackend,
		}); err != nil {
			return 0, fmt.Errorf("failed to decrement source quota: %w", err)
		}

		// --- Increment destination quota ---
		n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
			Amount:      locked.SizeBytes,
			BackendName: toBackend,
		})
		if err != nil {
			return 0, fmt.Errorf("failed to increment destination quota: %w", err)
		}
		if n == 0 {
			return 0, ErrNoSpaceAvailable
		}

		return locked.SizeBytes, nil
	})
}

// ListObjectsResult holds the result of a list objects query.
type ListObjectsResult struct {
	Objects               []ObjectLocation
	IsTruncated           bool
	NextContinuationToken string
}

// ListObjects returns objects matching the given prefix, sorted by key.
// Supports pagination via startAfter and maxKeys. Returns one extra row to
// detect truncation.
func (s *Store) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	// --- Escape LIKE wildcards in prefix ---
	escapedPrefix := likeEscaper.Replace(prefix)

	// Fetch one extra to detect truncation
	rows, err := s.queries.ListObjectsByPrefix(ctx, db.ListObjectsByPrefixParams{
		Prefix:     escapedPrefix,
		StartAfter: startAfter,
		MaxKeys:    int32(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}

	objects := toObjectLocations(rows)

	result := &ListObjectsResult{}
	if len(objects) > maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = objects[maxKeys-1].ObjectKey
		result.Objects = objects[:maxKeys]
	} else {
		result.Objects = objects
	}

	return result, nil
}

// ListExpiredObjects returns one row per unique key matching the given prefix
// whose created_at is older than cutoff, up to limit rows. Used by lifecycle
// expiration to find objects eligible for deletion.
func (s *Store) ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error) {
	escapedPrefix := likeEscaper.Replace(prefix)
	rows, err := s.queries.ListExpiredObjects(ctx, db.ListExpiredObjectsParams{
		Prefix:  escapedPrefix,
		Cutoff:  pgTimestamptz(cutoff),
		MaxKeys: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list expired objects: %w", err)
	}
	return toObjectLocations(rows), nil
}

// -------------------------------------------------------------------------
// DIRECTORY LISTING (DASHBOARD)
// -------------------------------------------------------------------------

// DirEntry holds aggregate stats for one immediate child of a directory prefix.
type DirEntry struct {
	Name      string `json:"name"`      // absolute path (e.g. "bucket/photos/")
	IsDir     bool   `json:"isDir"`     // true for directories
	FileCount int64  `json:"fileCount"` // number of files (recursive for dirs)
	TotalSize int64  `json:"totalSize"` // total bytes (recursive for dirs)
	Backend   string `json:"backend"`   // backend name (files only)
	CreatedAt string `json:"createdAt"` // formatted timestamp (files only)
}

// DirectoryListResult holds the response for a lazy-loaded directory listing.
type DirectoryListResult struct {
	Entries    []DirEntry `json:"entries"`
	HasMore    bool       `json:"hasMore"`
	NextCursor string     `json:"nextCursor"`
}

// ListDirectoryChildren returns the immediate children of a directory prefix
// with aggregate stats for subdirectories. Files include backend and creation
// time. Prefix must end with "/" (or be "" for root).
func (s *Store) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error) {
	if maxKeys <= 0 {
		maxKeys = 200
	}

	escapedPrefix := likeEscaper.Replace(prefix)

	// Get aggregate stats for all immediate children (dirs + files).
	stats, err := s.queries.GetDirectoryStats(ctx, escapedPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to get directory stats: %w", err)
	}

	// Get per-file detail for direct file children (paginated).
	fileRows, err := s.queries.ListDirectChildren(ctx, db.ListDirectChildrenParams{
		Prefix:     escapedPrefix,
		StartAfter: startAfter,
		MaxKeys:    int32(maxKeys + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list direct children: %w", err)
	}

	// Build a lookup of file details by relative name.
	type fileDetail struct {
		Backend   string
		CreatedAt string
	}
	fileLookup := make(map[string]fileDetail, len(fileRows))
	hasMore := len(fileRows) > maxKeys
	if hasMore {
		fileRows = fileRows[:maxKeys]
	}
	for _, row := range fileRows {
		relName := row.ObjectKey[len(prefix):]
		fileLookup[relName] = fileDetail{
			Backend:   row.BackendName,
			CreatedAt: row.CreatedAt.Time.Format("2006-01-02 15:04"),
		}
	}

	result := &DirectoryListResult{
		Entries: make([]DirEntry, 0, len(stats)),
	}

	for _, s := range stats {
		entry := DirEntry{
			Name:      prefix + s.Name,
			IsDir:     s.IsDir,
			FileCount: s.FileCount,
			TotalSize: s.TotalSize,
		}
		if !s.IsDir {
			if detail, ok := fileLookup[s.Name]; ok {
				entry.Backend = detail.Backend
				entry.CreatedAt = detail.CreatedAt
			} else {
				// File is outside the current page — skip it.
				continue
			}
		}
		result.Entries = append(result.Entries, entry)
	}

	if hasMore {
		lastKey := fileRows[len(fileRows)-1].ObjectKey
		result.HasMore = true
		result.NextCursor = lastKey
	}

	return result, nil
}

// -------------------------------------------------------------------------
// REPLICATION OPERATIONS
// -------------------------------------------------------------------------

// GetAllObjectLocations returns all copies of an object, ordered by created_at
// ascending (oldest/primary first). Used for read failover.
func (s *Store) GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetAllObjectLocations(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object locations: %w", err)
	}

	if len(rows) == 0 {
		return nil, ErrObjectNotFound
	}

	return toObjectLocations(rows), nil
}

// GetUnderReplicatedObjects finds objects with fewer copies than the target
// replication factor. Returns all rows for those objects so callers know which
// backends already have copies.
func (s *Store) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	rows, err := s.queries.GetUnderReplicatedObjects(ctx, db.GetUnderReplicatedObjectsParams{
		Factor:  int64(factor),
		MaxKeys: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects: %w", err)
	}

	return toObjectLocations(rows), nil
}

// GetUnderReplicatedObjectsExcluding finds objects with fewer copies than the
// target factor, ignoring copies on the excluded backends. Returns all rows
// for those objects so callers know the full picture.
func (s *Store) GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetUnderReplicatedObjectsExcluding(ctx, db.GetUnderReplicatedObjectsExcludingParams{
		Excluded: excludedBackends,
		Factor:   int64(factor),
		MaxKeys:  int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query under-replicated objects (excluding): %w", err)
	}

	return toObjectLocations(rows), nil
}

// RecordReplica inserts a replica copy of an object, but only if the source
// copy still exists. This prevents stale replicas when an object is overwritten
// or deleted during the (potentially slow) replication copy. Returns true if
// the replica was inserted, false if skipped.
func (s *Store) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) (bool, error) {
		// --- Conditional insert: only if source copy still exists ---
		inserted, err := qtx.InsertReplicaConditional(ctx, db.InsertReplicaConditionalParams{
			ObjectKey:     key,
			BackendName:   targetBackend,
			BackendName_2: sourceBackend,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to insert replica: %w", err)
		}

		if !inserted {
			return false, nil
		}

		// --- Increment quota for target backend ---
		n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
			Amount:      size,
			BackendName: targetBackend,
		})
		if err != nil {
			return false, fmt.Errorf("failed to update quota: %w", err)
		}
		if n == 0 {
			return false, ErrNoSpaceAvailable
		}

		return true, nil
	})
}

// GetOverReplicatedObjects finds objects with more copies than the target
// replication factor. Returns all rows for those objects so callers can
// score each copy and decide which to remove.
func (s *Store) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error) {
	var maxKeys int32
	switch {
	case limit <= 0:
		maxKeys = 0
	case limit > math.MaxInt32:
		maxKeys = math.MaxInt32
	default:
		maxKeys = int32(limit)
	}

	rows, err := s.queries.GetOverReplicatedObjects(ctx, db.GetOverReplicatedObjectsParams{
		Factor:  int64(factor),
		MaxKeys: maxKeys,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query over-replicated objects: %w", err)
	}

	return toObjectLocations(rows), nil
}

// CountOverReplicatedObjects returns the total number of objects with more
// copies than the target replication factor.
func (s *Store) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	count, err := s.queries.CountOverReplicatedObjects(ctx, int64(factor))
	if err != nil {
		return 0, fmt.Errorf("failed to count over-replicated objects: %w", err)
	}
	return count, nil
}

// RemoveExcessCopy deletes one copy of an object from the given backend inside
// a transaction, decrementing the backend quota atomically. The caller must
// have already performed FOR UPDATE locking and copy-count validation.
func (s *Store) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	return s.withTx(ctx, func(qtx *db.Queries) error {
		if err := qtx.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
			ObjectKey:   key,
			BackendName: backendName,
		}); err != nil {
			return fmt.Errorf("failed to delete excess copy: %w", err)
		}
		if err := qtx.DecrementQuota(ctx, db.DecrementQuotaParams{
			Amount:      size,
			BackendName: backendName,
		}); err != nil {
			return fmt.Errorf("failed to decrement quota: %w", err)
		}
		return nil
	})
}

// GetObjectCopiesForUpdate retrieves all copies of an object under a FOR
// UPDATE lock, suitable for use inside a transaction to prevent concurrent
// modification during over-replication cleanup.
func (s *Store) GetObjectCopiesForUpdate(ctx context.Context, key string) ([]ObjectLocation, error) {
	rows, err := s.queries.GetObjectCopiesForUpdate(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get copies for update: %w", err)
	}
	return toObjectLocations(rows), nil
}

// -------------------------------------------------------------------------
// MULTIPART UPLOAD OPERATIONS
// -------------------------------------------------------------------------

// MultipartUpload holds metadata for an in-progress multipart upload.
type MultipartUpload struct {
	UploadID    string
	ObjectKey   string
	BackendName string
	ContentType string
	Metadata    map[string]string
	CreatedAt   time.Time
}

// MultipartPart holds metadata for a single uploaded part.
// MultipartPart holds metadata for a single completed part of a multipart
// upload, including optional encryption metadata when encryption is enabled.
type MultipartPart struct {
	PartNumber    int
	ETag          string
	SizeBytes     int64
	CreatedAt     time.Time
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// CreateMultipartUpload records a new multipart upload in the database.
func (s *Store) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error {
	var metaJSON []byte
	if len(metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}
	err := s.queries.CreateMultipartUpload(ctx, db.CreateMultipartUploadParams{
		UploadID:    uploadID,
		ObjectKey:   key,
		BackendName: backend,
		ContentType: &contentType,
		Metadata:    metaJSON,
	})
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}
	return nil
}

// GetMultipartUpload retrieves metadata for a multipart upload.
func (s *Store) GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	row, err := s.queries.GetMultipartUpload(ctx, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMultipartUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get multipart upload: %w", err)
	}

	ct := ""
	if row.ContentType != nil {
		ct = *row.ContentType
	}

	mu := &MultipartUpload{
		UploadID:    row.UploadID,
		ObjectKey:   row.ObjectKey,
		BackendName: row.BackendName,
		ContentType: ct,
		CreatedAt:   row.CreatedAt.Time,
	}
	if len(row.Metadata) > 0 {
		if err := json.Unmarshal(row.Metadata, &mu.Metadata); err != nil {
			return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}
	return mu, nil
}

// RecordPart records a completed part for a multipart upload.
// S3 spec requires part numbers between 1 and 10000.
func (s *Store) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error {
	if partNumber < 1 || partNumber > 10000 {
		return fmt.Errorf("invalid part number %d: must be between 1 and 10000", partNumber)
	}
	params := db.UpsertPartParams{
		UploadID:   uploadID,
		PartNumber: int32(partNumber),
		Etag:       etag,
		SizeBytes:  size,
	}
	if enc != nil && enc.Encrypted {
		params.Encrypted = true
		params.EncryptionKey = enc.EncryptionKey
		params.KeyID = &enc.KeyID
		params.PlaintextSize = &enc.PlaintextSize
	}
	if err := s.queries.UpsertPart(ctx, params); err != nil {
		return fmt.Errorf("failed to record part: %w", err)
	}
	return nil
}

// GetParts returns all parts for a multipart upload, ordered by part number.
func (s *Store) GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error) {
	rows, err := s.queries.GetParts(ctx, uploadID)
	if err != nil {
		return nil, fmt.Errorf("failed to get parts: %w", err)
	}

	parts := make([]MultipartPart, len(rows))
	for i, row := range rows {
		p := MultipartPart{
			PartNumber: int(row.PartNumber),
			ETag:       row.Etag,
			SizeBytes:  row.SizeBytes,
			CreatedAt:  row.CreatedAt.Time,
			Encrypted:  row.Encrypted,
			EncryptionKey: row.EncryptionKey,
		}
		if row.KeyID != nil {
			p.KeyID = *row.KeyID
		}
		if row.PlaintextSize != nil {
			p.PlaintextSize = *row.PlaintextSize
		}
		parts[i] = p
	}
	return parts, nil
}

// DeleteMultipartUpload removes a multipart upload and its parts (cascading).
func (s *Store) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	err := s.queries.DeleteMultipartUpload(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("failed to delete multipart upload: %w", err)
	}
	return nil
}

// GetStaleMultipartUploads returns uploads older than the given duration.
func (s *Store) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := s.queries.GetStaleMultipartUploads(ctx, pgTimestamptz(cutoff))
	if err != nil {
		return nil, fmt.Errorf("failed to get stale uploads: %w", err)
	}

	uploads := make([]MultipartUpload, len(rows))
	for i, row := range rows {
		ct := ""
		if row.ContentType != nil {
			ct = *row.ContentType
		}
		mu := MultipartUpload{
			UploadID:    row.UploadID,
			ObjectKey:   row.ObjectKey,
			BackendName: row.BackendName,
			ContentType: ct,
			CreatedAt:   row.CreatedAt.Time,
		}
		if len(row.Metadata) > 0 {
			if err := json.Unmarshal(row.Metadata, &mu.Metadata); err != nil {
				return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
			}
		}
		uploads[i] = mu
	}
	return uploads, nil
}

// ListMultipartUploads returns in-progress multipart uploads whose key matches
// the given prefix, up to maxUploads entries.
func (s *Store) ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error) { // codecov:ignore -- requires live PostgreSQL, covered by integration tests
	escapedPrefix := likeEscaper.Replace(prefix)

	rows, err := s.queries.ListMultipartUploadsByPrefix(ctx, db.ListMultipartUploadsByPrefixParams{
		Prefix:     &escapedPrefix,
		MaxUploads: int32(maxUploads),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list multipart uploads: %w", err)
	}

	uploads := make([]MultipartUpload, len(rows))
	for i, row := range rows {
		ct := ""
		if row.ContentType != nil {
			ct = *row.ContentType
		}
		uploads[i] = MultipartUpload{
			UploadID:    row.UploadID,
			ObjectKey:   row.ObjectKey,
			ContentType: ct,
			CreatedAt:   row.CreatedAt.Time,
		}
	}
	return uploads, nil
}

// -------------------------------------------------------------------------
// SYNC OPERATIONS
// -------------------------------------------------------------------------

// ImportObject records a pre-existing object in the database without overwriting.
// Returns true if the object was imported, false if it already existed for this
// backend. Used by the sync subcommand to bring existing bucket objects under
// proxy management.
func (s *Store) ImportObject(ctx context.Context, key, backend string, size int64) (bool, error) {
	return withTxVal(s, ctx, func(qtx *db.Queries) (bool, error) {
		inserted, err := qtx.InsertObjectLocationIfNotExists(ctx, db.InsertObjectLocationIfNotExistsParams{
			ObjectKey:   key,
			BackendName: backend,
			SizeBytes:   size,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to import object %s: %w", key, err)
		}

		if !inserted {
			return false, nil
		}

		n, err := qtx.IncrementQuota(ctx, db.IncrementQuotaParams{
			Amount:      size,
			BackendName: backend,
		})
		if err != nil {
			return false, fmt.Errorf("failed to increment quota for %s: %w", backend, err)
		}
		if n == 0 {
			return false, ErrNoSpaceAvailable
		}

		return true, nil
	})
}

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE
// -------------------------------------------------------------------------

// BackendObjectStats returns the object count and total bytes stored on a backend.
func (s *Store) BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error) {
	row, err := s.queries.BackendObjectStats(ctx, backendName)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get backend object stats: %w", err)
	}
	return row.ObjectCount, row.TotalBytes, nil
}

// DeleteBackendData removes all database records for a backend in FK-safe order.
// Runs in a single transaction.
func (s *Store) DeleteBackendData(ctx context.Context, backendName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.queries.WithTx(tx)

	if err := qtx.DeleteCleanupQueueByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete cleanup queue: %w", err)
	}
	if err := qtx.DeleteMultipartUploadsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete multipart uploads: %w", err)
	}
	if err := qtx.DeleteObjectLocationsByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete object locations: %w", err)
	}
	if err := qtx.DeleteUsageByBackend(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete usage records: %w", err)
	}
	if err := qtx.DeleteQuota(ctx, backendName); err != nil {
		return fmt.Errorf("failed to delete quota: %w", err)
	}

	return tx.Commit(ctx)
}

// DeleteObjectLocation removes a single object_locations row for the given key
// and backend. Used by drain to remove source copies when a replica exists.
func (s *Store) DeleteObjectLocation(ctx context.Context, key, backendName string) error {
	return s.queries.DeleteObjectFromBackend(ctx, db.DeleteObjectFromBackendParams{
		ObjectKey:   key,
		BackendName: backendName,
	})
}

// -------------------------------------------------------------------------
// USAGE TRACKING
// -------------------------------------------------------------------------

// FlushUsageDeltas atomically adds accumulated usage deltas to the persistent
// usage row. Creates the row if it doesn't exist for this (backend, period).
func (s *Store) FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	return s.queries.FlushUsageDeltas(ctx, db.FlushUsageDeltasParams{
		BackendName:  backendName,
		Period:       period,
		ApiRequests:  apiRequests,
		EgressBytes:  egressBytes,
		IngressBytes: ingressBytes,
	})
}

// GetUsageForPeriod returns usage statistics for all backends in the given period.
func (s *Store) GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error) {
	rows, err := s.queries.GetUsageForPeriod(ctx, period)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]UsageStat, len(rows))
	for _, row := range rows {
		stats[row.BackendName] = UsageStat{
			APIRequests:  row.ApiRequests,
			EgressBytes:  row.EgressBytes,
			IngressBytes: row.IngressBytes,
		}
	}
	return stats, nil
}

// -------------------------------------------------------------------------
// ADVISORY LOCKS
// -------------------------------------------------------------------------

// Lock IDs for PostgreSQL advisory locks. Each background task uses a unique
// lock ID to prevent concurrent execution across multiple instances.
const (
	LockRebalancer       int64 = 1001
	LockReplicator       int64 = 1002
	LockCleanupQueue     int64 = 1003
	LockMultipartCleanup int64 = 1004
	LockLifecycle        int64 = 1005
	LockDrain            int64 = 1006
	LockUsageFlush       int64 = 1007
	LockOverReplication  int64 = 1008
	LockReconcile       int64 = 1009
)

// WithAdvisoryLock acquires a PostgreSQL session-level advisory lock on a
// dedicated connection from the pool. If the lock is acquired, fn runs and
// the connection is released (which releases the lock). If another session
// holds the lock, returns (false, nil). On DB error, returns (false, err).
func (s *Store) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to acquire connection for advisory lock: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired); err != nil {
		return false, fmt.Errorf("failed to attempt advisory lock: %w", err)
	}

	if !acquired {
		return false, nil
	}

	// Ensure the lock is released even if fn panics. Use a detached
	// context so the unlock succeeds even if the caller's ctx is cancelled.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockID)
	}()

	return true, fn(ctx)
}

// -------------------------------------------------------------------------
// KEY ROTATION (admin-only, not on MetadataStore interface)
// -------------------------------------------------------------------------

// EncryptedLocation holds a single encrypted object's key data for rotation.
type EncryptedLocation struct {
	ObjectKey     string
	BackendName   string
	EncryptionKey []byte
	KeyID         string
}

// ListEncryptedLocations returns a page of encrypted object locations filtered
// by key ID. Used during key rotation to find objects wrapped with the old key.
func (s *Store) ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]EncryptedLocation, error) {
	rows, err := s.queries.ListEncryptedLocations(ctx, db.ListEncryptedLocationsParams{
		KeyID:  &keyID,
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list encrypted locations: %w", err)
	}
	result := make([]EncryptedLocation, len(rows))
	for i, r := range rows {
		result[i] = EncryptedLocation{
			ObjectKey:     r.ObjectKey,
			BackendName:   r.BackendName,
			EncryptionKey: r.EncryptionKey,
		}
		if r.KeyID != nil {
			result[i].KeyID = *r.KeyID
		}
	}
	return result, nil
}

// UpdateEncryptionKey re-wraps a single object's encryption key. Used during
// key rotation to replace the old wrapped DEK with one wrapped by the new key.
func (s *Store) UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error {
	return s.queries.UpdateEncryptionKey(ctx, db.UpdateEncryptionKeyParams{
		ObjectKey:     objectKey,
		BackendName:   backendName,
		EncryptionKey: newEncryptionKey,
		KeyID:         &newKeyID,
	})
}

// UnencryptedLocation holds a single unencrypted object's metadata for the
// encrypt-existing admin operation.
type UnencryptedLocation struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
}

// ListUnencryptedLocations returns a page of unencrypted object locations.
// Used by the encrypt-existing admin endpoint to find objects that need encryption.
func (s *Store) ListUnencryptedLocations(ctx context.Context, limit, offset int) ([]UnencryptedLocation, error) {
	rows, err := s.queries.ListUnencryptedLocations(ctx, db.ListUnencryptedLocationsParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list unencrypted locations: %w", err)
	}
	result := make([]UnencryptedLocation, len(rows))
	for i, r := range rows {
		result[i] = UnencryptedLocation{
			ObjectKey:   r.ObjectKey,
			BackendName: r.BackendName,
			SizeBytes:   r.SizeBytes,
		}
	}
	return result, nil
}

// MarkObjectEncrypted updates a single object location to record that it has
// been encrypted. Sets the encryption flag, wrapped DEK, key ID, plaintext
// size, and updates size_bytes to the ciphertext size.
func (s *Store) MarkObjectEncrypted(ctx context.Context, objectKey, backendName string, encryptionKey []byte, keyID string, plaintextSize, ciphertextSize int64) error {
	return s.queries.MarkObjectEncrypted(ctx, db.MarkObjectEncryptedParams{
		ObjectKey:     objectKey,
		BackendName:   backendName,
		EncryptionKey: encryptionKey,
		KeyID:         &keyID,
		PlaintextSize: &plaintextSize,
		SizeBytes:     ciphertextSize,
	})
}

// pgTimestamptz converts a time.Time to pgtype.Timestamptz for use with sqlc.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
