// -------------------------------------------------------------------------------
// SQLite Helpers - Tests
//
// Author: Alex Freidah
//
// Round-trip tests for the nullable column helpers in helpers.go. These
// pin the contract that "" / 0 maps to NULL on the write side and back to
// "" / 0 on the read side, with non-zero values flowing through unchanged.
// -------------------------------------------------------------------------------

package sqlite

import (
	"database/sql"
	"testing"
)

// TestNullableString_RoundTrip pins the empty/non-empty mapping in both
// directions so adding a new nullable string column cannot accidentally
// produce a NULL vs empty-string mismatch between insert and scan paths.
func TestNullableString_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		valid bool
	}{
		{"empty", "", false},
		{"non-empty", "hello", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nullableString(tc.in)
			if got.Valid != tc.valid {
				t.Errorf("Valid = %v, want %v", got.Valid, tc.valid)
			}
			if got.Valid && got.String != tc.in {
				t.Errorf("String = %q, want %q", got.String, tc.in)
			}
			if back := nullStringValue(got); back != tc.in {
				t.Errorf("round-trip = %q, want %q", back, tc.in)
			}
		})
	}
}

// TestNullableInt64_RoundTrip pins the zero/non-zero mapping in both
// directions.
func TestNullableInt64_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    int64
		valid bool
	}{
		{"zero", 0, false},
		{"positive", 42, true},
		{"negative", -7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nullableInt64(tc.in)
			if got.Valid != tc.valid {
				t.Errorf("Valid = %v, want %v", got.Valid, tc.valid)
			}
			if got.Valid && got.Int64 != tc.in {
				t.Errorf("Int64 = %d, want %d", got.Int64, tc.in)
			}
			if back := nullInt64Value(got); back != tc.in {
				t.Errorf("round-trip = %d, want %d", back, tc.in)
			}
		})
	}
}

// TestNullStringValue_InvalidReturnsEmpty pins the read-side fallback
// for SQL NULL coming back from a column whose Go target is sql.NullString.
func TestNullStringValue_InvalidReturnsEmpty(t *testing.T) {
	t.Parallel()
	// String field set but Valid=false simulates a NULL column whose
	// scan target was reused: the helper must ignore the stale string.
	got := nullStringValue(sql.NullString{String: "leftover", Valid: false})
	if got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

// TestNullInt64Value_InvalidReturnsZero pins the read-side fallback for
// NULL int64 columns.
func TestNullInt64Value_InvalidReturnsZero(t *testing.T) {
	t.Parallel()
	got := nullInt64Value(sql.NullInt64{Int64: 99, Valid: false})
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
