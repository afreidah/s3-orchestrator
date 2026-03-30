// -------------------------------------------------------------------------------
// SQLite Helpers - Shared Utilities for the SQLite Store
//
// Author: Alex Freidah
//
// Common helper functions used across multiple SQLite store files: timestamp
// formatting, time parsing, and dynamic SQL placeholder generation.
// -------------------------------------------------------------------------------

package sqlite

import "time"

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

