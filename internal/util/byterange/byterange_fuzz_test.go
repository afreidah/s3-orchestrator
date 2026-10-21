// -------------------------------------------------------------------------------
// byterange - Fuzz Tests
//
// Author: Alex Freidah
//
// Fuzz tests for Parse covering malformed, adversarial, and edge-case Range
// header values. Validates structural invariants on output.
// -------------------------------------------------------------------------------

package byterange

import (
	"testing"
)

// FuzzParse exercises the Range header parser with arbitrary input strings
// and object sizes. Asserts that successful parses always produce valid byte
// offsets.
func FuzzParse(f *testing.F) {
	f.Add("bytes=0-99", int64(100))
	f.Add("bytes=0-0", int64(1))
	f.Add("bytes=-50", int64(100))
	f.Add("bytes=50-", int64(100))
	f.Add("bytes=0-999999", int64(100))
	f.Add("", int64(100))
	f.Add("bytes=", int64(100))
	f.Add("bytes=-", int64(100))
	f.Add("bytes=abc-def", int64(100))
	f.Add("bytes=0-99", int64(1))
	f.Add("notbytes=0-99", int64(100))
	f.Add("bytes=99-0", int64(100))
	f.Add("bytes=0-0-0", int64(100))
	f.Add("bytes=-0", int64(100))
	f.Add("bytes=9999999999999999999-0", int64(100))

	f.Fuzz(func(t *testing.T, header string, size int64) {
		// Parse assumes size > 0 (callers validate this).
		if size <= 0 {
			return
		}

		start, end, ok := Parse(header, size)
		if !ok {
			return
		}

		if start < 0 {
			t.Errorf("Parse(%q, %d): start=%d is negative", header, size, start)
		}
		if end < start {
			t.Errorf("Parse(%q, %d): end=%d < start=%d", header, size, end, start)
		}
		if end >= size {
			t.Errorf("Parse(%q, %d): end=%d >= size", header, size, end)
		}
	})
}
