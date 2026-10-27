// -------------------------------------------------------------------------------
// SQLite Request Error Tests
//
// Author: Alex Freidah
//
// Drives a real engine into each kind of failure: a constraint violation is a
// request error that leaves the circuit breaker alone, a file that is not a
// database is not, and neither is an error that never came from the engine.
// The store's Ping answers while its breaker is open, which is what the
// breaker's recovery relies on.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestRequestError_ConstraintViolation verifies a duplicate key from a healthy
// engine is a request error.
func TestRequestError_ConstraintViolation(t *testing.T) {
	t.Parallel()
	d := openMemDB(t)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `CREATE TABLE k (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO k VALUES ('a')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := d.ExecContext(ctx, `INSERT INTO k VALUES ('a')`)
	if err == nil || !RequestError(err) {
		t.Errorf("duplicate insert error %v: RequestError = false, want true", err)
	}
}

// TestRequestError_NotADatabase verifies a file the engine cannot use as a
// database is not a request error, and neither is an error from elsewhere.
func TestRequestError_NotADatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database, only text padded out to a page"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	_, err = d.ExecContext(context.Background(), `SELECT * FROM sqlite_master`)
	if err == nil || RequestError(err) {
		t.Errorf("not-a-database error %v: RequestError = true, want false", err)
	}
	if RequestError(errors.New("sql: database is closed")) {
		t.Error("an error from outside the engine read as a request error")
	}
}

// TestStore_PingBypassesOpenBreaker verifies Ping answers while the store's
// breaker refuses every statement.
func TestStore_PingBypassesOpenBreaker(t *testing.T) {
	t.Parallel()
	cb := breaker.NewCircuitBreaker(breaker.Config{
		Name: "test", Threshold: 1, Timeout: time.Minute,
		IsError: func(err error) bool { return err != nil }, Sentinel: core.ErrDBUnavailable,
	})
	s, err := NewStore(context.Background(), &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, cb, testInstanceID)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	_ = cb.PostCheck(errors.New("disk I/O error"))
	if cb.IsHealthy() {
		t.Fatal("setup: breaker should be open")
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping with the breaker open = %v, want nil", err)
	}
}
