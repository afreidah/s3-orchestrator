// -------------------------------------------------------------------------------
// SQLite Store - Embedded Database Backend for Single-Instance Deployments
//
// Author: Alex Freidah
//
// Implements every core role interface and the three admin role interfaces
// (LifecycleAdmin, EncryptionAdmin, NotificationOutbox) using an embedded
// SQLite database via modernc.org/sqlite (pure Go, no CGo). Provides WAL
// mode for concurrent reads, process-local mutex for advisory lock
// emulation, and automatic schema migration on first start.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	_ "modernc.org/sqlite" // register pure-Go SQLite driver for database/sql
)

// Store implements every core role interface plus LifecycleAdmin,
// EncryptionAdmin, and NotificationOutbox using SQLite.
type Store struct {
	db *sql.DB
	mu sync.Mutex // advisory lock emulation for single-instance
}

// NewStore opens a SQLite database at the configured path, applies pragmas
// for WAL mode and foreign key enforcement, and runs migrations.
func NewStore(ctx context.Context, dbCfg *config.DatabaseConfig) (*Store, error) {
	db, err := sql.Open("sqlite", dbCfg.Path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite is single-writer; serialize all writes through one connection.
	db.SetMaxOpenConns(1)

	// Apply performance and correctness pragmas.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.RunMigrations(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() {
	s.db.Close()
}

// withTx executes fn inside a transaction. Commits on success, rolls back on error.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// WithTx satisfies core.Runner by opening a transaction, wrapping it in
// a sqliteTxAdapter, and invoking fn. Commits on a nil return; rolls
// back otherwise. Lets engine-agnostic core helpers orchestrate
// multi-statement operations against the SQLite engine.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx core.TxAdapter) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	adapter := &sqliteTxAdapter{tx: tx}
	if err := fn(ctx, adapter); err != nil {
		return err
	}
	return tx.Commit()
}

// Compile-time check that *Store satisfies core.Runner.
var _ core.Runner = (*Store)(nil)

// WithAdvisoryLock emulates PostgreSQL advisory locks using a process-local
// mutex. For single-instance SQLite deployments, this is correct — there are
// no competing instances. Returns (false, nil) if the lock is already held
// by another goroutine.
func (s *Store) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if !s.mu.TryLock() {
		return false, nil
	}
	defer s.mu.Unlock()
	return true, fn(ctx)
}

// Compile-time interface checks — *Store must satisfy every narrow role
// interface and the three admin role interfaces.
var (
	_ core.ObjectStore           = (*Store)(nil)
	_ core.QuotaStore            = (*Store)(nil)
	_ core.MultipartStore        = (*Store)(nil)
	_ core.ReplicationStore      = (*Store)(nil)
	_ core.CleanupStore          = (*Store)(nil)
	_ core.IntegrityStore        = (*Store)(nil)
	_ core.ExpiredObjectsLister  = (*Store)(nil)
	_ core.BackendLifecycleStore = (*Store)(nil)
	_ core.DashboardStore        = (*Store)(nil)
	_ core.UsageFlusher          = (*Store)(nil)
	_ core.AdvisoryLocker        = (*Store)(nil)
	_ core.LifecycleAdmin        = (*Store)(nil)
	_ core.EncryptionAdmin       = (*Store)(nil)
	_ core.NotificationOutbox    = (*Store)(nil)
)
