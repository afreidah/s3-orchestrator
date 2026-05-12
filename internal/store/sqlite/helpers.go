// -------------------------------------------------------------------------------
// SQLite Helpers - Shared Utilities for the SQLite Store
//
// Author: Alex Freidah
//
// Common helper functions used across multiple SQLite store files: timestamp
// formatting, time parsing, and nullable column conversions for round-tripping
// optional string/int64 fields between core domain types and sql.Null*.
// -------------------------------------------------------------------------------

package sqlite

import (
	"database/sql"
	"time"
)

// -------------------------------------------------------------------------
// TIMESTAMP HELPERS
// -------------------------------------------------------------------------

// now returns the current time as an RFC3339Nano string for SQLite storage.
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// parseTime parses an RFC3339Nano timestamp string from SQLite into a time.Time.
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// -------------------------------------------------------------------------
// NULLABLE COLUMN HELPERS
// -------------------------------------------------------------------------

// nullableString returns sql.NullString{Valid:false} when s is empty so
// the column stores SQL NULL rather than the zero value. Inverse of
// nullStringValue.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullableInt64 returns sql.NullInt64{Valid:false} when n is zero so the
// column stores SQL NULL rather than the zero value. Inverse of
// nullInt64Value.
func nullableInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

// nullStringValue returns s.String when s.Valid, otherwise "". Pairs
// with nullableString for round-tripping optional string columns
// without the inline "if x.Valid { dst = x.String }" pattern at every
// scan site.
func nullStringValue(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

// nullInt64Value returns n.Int64 when n.Valid, otherwise 0. Pairs with
// nullableInt64.
func nullInt64Value(n sql.NullInt64) int64 {
	if !n.Valid {
		return 0
	}
	return n.Int64
}
