// -------------------------------------------------------------------------------
// commalist - Comma-Separated List Splitting
//
// Author: Alex Freidah
//
// The one split a list header or flag value goes through: entries trimmed of
// surrounding space, and empty entries dropped, so "a, ,b," reads as [a b].
// -------------------------------------------------------------------------------

package commalist

import "strings"

// Split returns the trimmed, non-empty entries of a comma-separated list, in
// order. A blank value returns nil.
func Split(v string) []string {
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if entry := strings.TrimSpace(part); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
