// -------------------------------------------------------------------------------
// Postgres Request Error Tests
//
// Author: Alex Freidah
//
// Pins which Postgres errors leave the database circuit breaker alone: the
// server refusing one request (a duplicate key, a serialization conflict, a
// cancelled statement, a raised exception) and a row the client could not scan
// do; the states that say the server cannot serve, and errors that never
// reached it, do not.
// -------------------------------------------------------------------------------

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestRequestError_ClassifiesBySQLState covers a request-level refusal and an
// unavailability state from each class the breaker counts.
func TestRequestError_ClassifiesBySQLState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"23505", true},  // unique violation
		{"23503", true},  // foreign key violation
		{"40001", true},  // serialization failure
		{"57014", true},  // statement cancelled or timed out
		{"P0001", true},  // raised exception
		{"08006", false}, // connection failure
		{"28P01", false}, // invalid password
		{"3D000", false}, // database does not exist
		{"53300", false}, // too many connections
		{"57P01", false}, // admin shutdown
		{"58030", false}, // I/O error
		{"XX000", false}, // internal error
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("insert object location: %w", &pgconn.PgError{Code: tc.code})
			if got := RequestError(err); got != tc.want {
				t.Errorf("RequestError(%s) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestRequestError_ScanAndTransport verifies a row the client could not scan is
// a request error, and an error that never reached the server is not.
func TestRequestError_ScanAndTransport(t *testing.T) {
	t.Parallel()
	if !RequestError(fmt.Errorf("get object: %w", pgx.ScanArgError{ColumnIndex: 1, Err: errors.New("cannot scan NULL")})) {
		t.Error("a scan error counted as unavailability")
	}
	if RequestError(errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")) {
		t.Error("a refused connection read as a request error")
	}
}
