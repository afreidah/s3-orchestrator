// -------------------------------------------------------------------------------
// HTTP Query Parameter Helpers
//
// Author: Alex Freidah
//
// Shared parsing for the optional numeric query parameters the admin surfaces
// take (batch_size, max, delay_ms). Every one of them means "leave it to the
// server" when absent, so they collapse malformed and missing to the same zero
// rather than rejecting the request: an operator who mistypes a cap gets the
// documented default, not a 400 halfway through a maintenance window.
// -------------------------------------------------------------------------------

package httputil

import "strconv"

// QueryPositiveInt parses a query value as a positive int, returning 0 for
// anything absent, unparseable, or not positive.
func QueryPositiveInt(v string) int {
	if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
		return int(n)
	}
	return 0
}
