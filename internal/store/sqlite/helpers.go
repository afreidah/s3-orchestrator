// -------------------------------------------------------------------------------
// SQLite Helpers - Shared Utilities for the SQLite Store
//
// Author: Alex Freidah
//
// Common helper functions used across multiple SQLite store files: timestamp
// formatting, time parsing, nullable column conversions for round-tripping
// optional string/int64 fields between core domain types and sql.Null*, and the
// row collection every query body drains its result through.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TIMESTAMP HELPERS
// -------------------------------------------------------------------------

// timestampFormat is the canonical on-disk shape for every timestamp column.
//
// SQLite stores these as TEXT, so ORDER BY and every range comparison on them
// is lexicographic, and text order has to agree with chronological order.
// time.RFC3339Nano cannot be used for writes because it strips trailing zeros:
// the fractional part varies in width, and wherever the earlier value is a
// prefix of the later one the comparison inverts, because 'Z' (0x5A) sorts
// above '0' (0x30).
//
//	"...00.5Z"  >  "...00.50000001Z"   as text
//	"...00.5Z"  <  "...00.50000001Z"   in time
//
// Nine digits, always padded, keeps the two orders identical. Milliseconds
// would also be fixed width but are too coarse: writes made within the same
// millisecond collapse into ties, and the scrub queue then falls through to its
// object_key tiebreak rather than ordering by age.
const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// canonicalTimestampLen is the width every stored timestamp has once written in
// timestampFormat: "2006-01-02T15:04:05.000000000Z". Migration 0006 uses it to
// recognise rows it has already normalised, which is what makes re-running free.
const canonicalTimestampLen = 30

// now returns the current time in the canonical on-disk shape.
func now() string {
	return formatTime(time.Now())
}

// formatTime renders t in the canonical on-disk shape. Every write of a
// timestamp column goes through here or through now(), so no column ever holds
// a mix of widths.
func formatTime(t time.Time) string {
	return t.UTC().Format(timestampFormat)
}

// parseTime parses a stored timestamp, naming the value it could not read.
// Reads accept RFC3339Nano rather than the canonical format so rows written
// before this change, and rows written by a column DEFAULT, still parse.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: %w", s, err)
	}
	return t, nil
}

// parseNullableTime parses a nullable timestamp column, returning nil for a
// NULL and an error for a value that does not parse.
func parseNullableTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
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

// deref returns *p, or the zero value when p is nil, which is how a scanned
// NULL column reaches the model.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// jsonList renders a list of names or ids as the JSON array json_each expands,
// so a query takes a whole list as one fixed parameter. An empty or nil list is
// "[]", since json_each reads "null" as a single NULL element.
func jsonList[T string | int64](values []T) string {
	if len(values) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(values) // a list of strings or integers always encodes
	return string(b)
}

// -------------------------------------------------------------------------
// ROW COLLECTION
// -------------------------------------------------------------------------

// rowsObjectLocations names object_locations rows for the iteration error the
// several queries that read them share.
const rowsObjectLocations = "object locations"

// collectRows drains rows into a slice, calling scan once per row, then closes
// the rows and checks rows.Err. No rows is an empty slice rather than nil, as
// sqlc returns on Postgres. Scan errors are returned untouched; what names the
// rows only in the iteration error.
func collectRows[T any](rows *sql.Rows, what string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	defer func() { _ = rows.Close() }()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate %s: %w", what, err)
	}
	return out, nil
}

// countRows runs a single-value COUNT query and labels its failure. Every
// depth and count in this package is otherwise the same three lines around a
// different string.
func (s *Store) countRows(ctx context.Context, what, query string, args ...any) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count %s: %w", what, err)
	}
	return count, nil
}

// execChanged runs a write outside any transaction and reports whether it
// changed any row; see sqliteTxAdapter.execChanged.
func (s *Store) execChanged(ctx context.Context, what, query string, args ...any) (bool, error) {
	return s.direct().execChanged(ctx, what, query, args...)
}

// exec runs a write outside any transaction; see sqliteTxAdapter.exec.
func (s *Store) exec(ctx context.Context, what, query string, args ...any) error {
	return s.direct().exec(ctx, what, query, args...)
}

// collectMap is collectRows for the queries that come back keyed - one row per
// backend, per period, per name - where the scan yields the key alongside the
// value it maps to.
func collectMap[T any](rows *sql.Rows, what string, scan func(*sql.Rows) (string, T, error)) (map[string]T, error) {
	defer func() { _ = rows.Close() }()
	out := make(map[string]T)
	for rows.Next() {
		key, value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate %s: %w", what, err)
	}
	return out, nil
}

// scanNameValue reads the (name, number) pair every per-backend total in this
// package selects. A NULL sum reads as zero, since a backend with no rows has
// no bytes rather than an unknown number of them.
func scanNameValue(rows *sql.Rows) (string, int64, error) {
	var (
		name  string
		value sql.NullInt64
	)
	if err := rows.Scan(&name, &value); err != nil {
		return "", 0, fmt.Errorf("failed to scan per-backend total: %w", err)
	}
	return name, nullInt64Value(value), nil
}

// -------------------------------------------------------------------------
// IDENTITY COLUMN HELPERS
// -------------------------------------------------------------------------

// identityColumns renders an optional identity as its three nullable columns,
// each NULL where core.IdentityColumns leaves the value empty.
func identityColumns(id *core.ObjectIdentity) (etag, contentType, userMetadata sql.NullString) {
	e, ct, meta := core.IdentityColumns(id)
	return nullableString(e), nullableString(ct), nullableString(string(meta))
}

// identityFromColumns rebuilds the identity a row carries, or nil when it
// carries none.
func identityFromColumns(etag, contentType, userMetadata sql.NullString) *core.ObjectIdentity {
	id, err := core.IdentityFromColumns(nullStringValue(etag), nullStringValue(contentType), []byte(nullStringValue(userMetadata)))
	if err != nil {
		return nil
	}
	return id
}
