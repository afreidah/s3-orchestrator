// -------------------------------------------------------------------------------
// Database Circuit Breaker Tests
//
// Author: Alex Freidah
//
// Pins what counts against the database breaker: a broken connection does; a
// caller giving up, an answer such as no rows or no space, and the driver's
// request errors do not. A healthy database answering duplicate keys must not
// open the breaker however many it answers.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
)

// errDuplicateKey is a healthy Postgres refusing one insert.
var errDuplicateKey = fmt.Errorf("insert object location: %w", &pgconn.PgError{Code: "23505"})

// TestUnavailable_CountsOnlyAvailabilityFailures covers each kind of error the
// filter passes over and a broken connection, which it counts.
func TestUnavailable_CountsOnlyAvailabilityFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"caller cancelled", fmt.Errorf("query: %w", context.Canceled), false},
		{"caller deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), false},
		{"sql no rows", sql.ErrNoRows, false},
		{"pgx no rows", pgx.ErrNoRows, false},
		{"no space", core.ErrInsufficientStorage, false},
		{"s3 error", &core.S3Error{StatusCode: 404, Code: "NoSuchKey"}, false},
		{"duplicate key", errDuplicateKey, false},
		{"connection refused", errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"), true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := unavailable(tc.err, postgres.RequestError); got != tc.want {
				t.Errorf("unavailable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNewDatabaseBreaker_DuplicateKeysDoNotOpen verifies a run of duplicate
// keys past the threshold leaves the breaker closed, while the same run of
// broken connections opens it.
func TestNewDatabaseBreaker_DuplicateKeysDoNotOpen(t *testing.T) {
	t.Parallel()
	cb := NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3}, postgres.RequestError)
	for range 10 {
		_ = cb.PostCheck(errDuplicateKey)
	}
	if !cb.IsHealthy() {
		t.Fatal("duplicate keys opened the database breaker")
	}
	for range 3 {
		_ = cb.PostCheck(errors.New("read tcp: connection reset by peer"))
	}
	if cb.IsHealthy() {
		t.Error("broken connections past the threshold left the breaker closed")
	}
}
