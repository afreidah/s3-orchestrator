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

// Package sqlite implements every core store role plus the admin
// roles using an embedded SQLite database via modernc.org/sqlite. WAL
// mode handles concurrent reads, a process-local mutex emulates
// advisory locks, and the schema migrates on first start.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	_ "modernc.org/sqlite" // register pure-Go SQLite driver for database/sql
)

// Store implements every core role interface plus LifecycleAdmin,
// EncryptionAdmin, and NotificationOutbox using SQLite. The db field is
// typed as the local dbAPI interface so the production wiring can hand
// the store either a raw *sql.DB or a CB-wrapped one transparently.
// rawDB is the same handle without the wrapper; transactional code
// paths begin tx through cbBeginTx so the rollback defer can live at
// the same call site as the begin.
// The embedded core.TxOps supplies the methods whose whole body is a core
// transaction over this store as Runner; the methods declared here are the
// SQLite-specific queries.
type Store struct {
	core.TxOps

	db    dbAPI
	rawDB *sql.DB
	cb    *breaker.CircuitBreaker
	mu    sync.Mutex // advisory lock emulation for single-instance
}

// NewStore opens a SQLite database at the configured path, applies pragmas
// for WAL mode and foreign key enforcement, and runs migrations. When cb
// is non-nil, every statement the store fires goes through PreCheck/
// PostCheck. Pass nil to skip CB wrapping (test fixtures).
func NewStore(ctx context.Context, dbCfg *config.DatabaseConfig, cb *breaker.CircuitBreaker) (*Store, error) {
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

	s := &Store{db: wrapDB(db, cb), rawDB: db, cb: cb}
	s.TxOps = core.NewTxOps(s)
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

// withTx executes fn inside a transaction. Commits on success, rolls
// back on error. Tx lifecycle and breaker pre/post-checks live in
// cbWithTx; this method just hands fn through.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return cbWithTx(ctx, s.rawDB, s.cb, fn)
}

// WithTx satisfies core.Runner by opening a transaction, wrapping it in
// a sqliteTxAdapter, and invoking fn. Commits on a nil return; rolls
// back otherwise. Lets engine-agnostic core helpers orchestrate
// multi-statement operations against the SQLite engine.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx core.TxAdapter) error) error {
	return cbWithTx(ctx, s.rawDB, s.cb, func(tx *sql.Tx) error {
		return fn(ctx, &sqliteTxAdapter{tx: tx})
	})
}

// Compile-time check that *Store satisfies core.Runner.
var _ core.Runner = (*Store)(nil)

// WithAdvisoryLock emulates PostgreSQL advisory locks using a process-local
// mutex. For single-instance SQLite deployments, this is correct  -  there are
// no competing instances. Returns (false, nil) if the lock is already held
// by another goroutine.
func (s *Store) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if !s.mu.TryLock() {
		return false, nil
	}
	defer s.mu.Unlock()
	return true, fn(ctx)
}

// Compile-time check that *Store satisfies the wide metadata-store
// contract every consumer depends on.
var _ core.MetadataStore = (*Store)(nil)
