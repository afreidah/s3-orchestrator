// -------------------------------------------------------------------------------
// Range Parser Fuzz Tests - HTTP Range Header Parsing
//
// Author: Alex Freidah
//
// Fuzz tests for parsePlaintextRange covering malformed, adversarial, and
// edge-case Range header values. Validates structural invariants on output.
// -------------------------------------------------------------------------------

package proxy

import (
	"testing"
)

// FuzzParsePlaintextRange exercises the Range header parser with arbitrary
// input strings and plaintext sizes. Asserts that successful parses always
// produce valid byte offsets.
func FuzzParsePlaintextRange(f *testing.F) {
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

	f.Fuzz(func(t *testing.T, rangeHeader string, plaintextSize int64) {
		// parsePlaintextRange assumes plaintextSize > 0 (callers validate this).
		if plaintextSize <= 0 {
			return
		}

		start, end, ok := parsePlaintextRange(rangeHeader, plaintextSize)
		if !ok {
			return
		}

		// Start must be non-negative.
		if start < 0 {
			t.Errorf("parsePlaintextRange(%q, %d): start=%d is negative", rangeHeader, plaintextSize, start)
		}

		// End must be >= start for a valid range.
		if end < start {
			t.Errorf("parsePlaintextRange(%q, %d): end=%d < start=%d", rangeHeader, plaintextSize, end, start)
		}

		// End must not exceed the last valid byte offset.
		if plaintextSize > 0 && end >= plaintextSize {
			t.Errorf("parsePlaintextRange(%q, %d): end=%d >= plaintextSize", rangeHeader, plaintextSize, end)
		}
	})
}
