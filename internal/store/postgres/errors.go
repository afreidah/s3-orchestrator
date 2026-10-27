// -------------------------------------------------------------------------------
// Postgres Request Errors
//
// Author: Alex Freidah
//
// Tells the database circuit breaker which Postgres errors are about one
// request rather than the server. A server that answers a statement with an
// error is up: a duplicate key, a serialization conflict, a check violation or
// a raised exception says the request was refused, not that the database is
// down. Only the SQLSTATE classes that mean the server cannot serve at all -
// a broken connection, refused credentials, exhausted resources, shutdown, an
// I/O or internal failure - count against the breaker.
// -------------------------------------------------------------------------------

package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// unavailableStates are the SQLSTATE classes and codes that mean the server
// cannot serve: connection exception (08), invalid authorization (28),
// insufficient resources (53), operator intervention short of a cancelled
// statement (57P), system error (58), internal error (XX), and a database
// that does not exist (3D000).
var unavailableStates = []string{"08", "28", "53", "57P", "58", "XX", "3D000"}

// RequestError reports whether err is the server refusing one request while
// staying able to serve others, or a row the client could not scan. Neither
// says the database is down, so neither counts against the circuit breaker.
func RequestError(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return !unavailableState(pgErr.Code)
	}
	_, scan := errors.AsType[pgx.ScanArgError](err)
	return scan
}

// unavailableState reports whether a SQLSTATE says the server cannot serve.
func unavailableState(code string) bool {
	for _, prefix := range unavailableStates {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}
