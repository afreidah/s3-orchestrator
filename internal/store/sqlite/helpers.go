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

// parseTime parses a stored timestamp. Reads accept RFC3339Nano rather than the
// canonical format so rows written before this change, and rows written by a
// column DEFAULT, still parse.
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

// boolToInt renders a Go bool as the 0/1 integer SQLite stores booleans as.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// -------------------------------------------------------------------------
// IDENTITY COLUMN HELPERS
// -------------------------------------------------------------------------

// identityETag, identityContentType and identityMetadataJSON render the three
// identity columns from an optional *core.ObjectIdentity. A nil identity
// writes NULL to all three, which is the row an object whose ETag was never
// computed carries.
func identityETag(id *core.ObjectIdentity) sql.NullString {
	if id == nil {
		return sql.NullString{}
	}
	return nullableString(id.ETag)
}

func identityContentType(id *core.ObjectIdentity) sql.NullString {
	if id == nil {
		return sql.NullString{}
	}
	return nullableString(id.ContentType)
}

// A marshal failure would mean a map[string]string that cannot be JSON, which
// does not exist; the column stays NULL rather than failing a write over
// metadata a later read can re-learn.
func identityMetadataJSON(id *core.ObjectIdentity) sql.NullString {
	if id == nil {
		return sql.NullString{}
	}
	b, err := core.EncodeUserMetadata(id.UserMetadata)
	if err != nil || len(b) == 0 {
		return sql.NullString{}
	}
	return nullableString(string(b))
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
