// -------------------------------------------------------------------------------
// SQLite Request Errors
//
// Author: Alex Freidah
//
// Tells the database circuit breaker which SQLite errors are about one
// statement rather than the database file. A constraint violation, a busy or
// locked database, or a mismatched value is the engine refusing one statement
// while it stays able to serve the rest. Only the result codes that mean the
// file cannot be used - it cannot be opened, read, written or trusted, or the
// engine has run out of memory - count against the breaker.
// -------------------------------------------------------------------------------

package sqlite

import (
	"errors"
	"slices"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// unavailableCodes are the primary result codes that mean the database file
// cannot be used.
var unavailableCodes = []int{
	sqlite3.SQLITE_CANTOPEN,
	sqlite3.SQLITE_CORRUPT,
	sqlite3.SQLITE_FULL,
	sqlite3.SQLITE_IOERR,
	sqlite3.SQLITE_NOMEM,
	sqlite3.SQLITE_NOTADB,
	sqlite3.SQLITE_PERM,
	sqlite3.SQLITE_READONLY,
}

// RequestError reports whether err is the engine refusing one statement while
// staying able to serve others, so it does not count against the circuit
// breaker. The primary code is the low byte of an extended result code.
func RequestError(err error) bool {
	sqliteErr, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return false
	}
	return !slices.Contains(unavailableCodes, sqliteErr.Code()&0xff)
}
