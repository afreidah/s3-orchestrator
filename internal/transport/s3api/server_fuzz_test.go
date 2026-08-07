// -------------------------------------------------------------------------------
// Server Fuzz Tests - Request ID Validation
//
// Author: Alex Freidah
//
// Fuzz tests for request ID validation. Ensures isValidRequestID enforces
// length bounds and hex-only characters for all inputs, preventing log
// injection and header injection via client-supplied request IDs.
// -------------------------------------------------------------------------------

package s3api

import "testing"

// FuzzIsValidRequestID exercises isValidRequestID against arbitrary
// strings. When the validator returns true, the input must satisfy the
// length and hex-only invariants enforced by the production code.
func FuzzIsValidRequestID(f *testing.F) {
	f.Add("abcdef1234567890")
	f.Add("")
	f.Add("ABCDEF")
	f.Add("not-hex!")
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 65 chars
	f.Add("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")    // 64 chars

	f.Fuzz(func(t *testing.T, id string) {
		if !isValidRequestID(id) {
			return
		}
		assertRequestIDLengthValid(t, id)
		assertRequestIDHexOnly(t, id)
	})
}

// assertRequestIDLengthValid fails the test when an accepted request ID
// falls outside the (0, 64] length window the validator advertises.
func assertRequestIDLengthValid(t *testing.T, id string) {
	t.Helper()
	if len(id) == 0 || len(id) > 64 {
		t.Errorf("isValidRequestID(%q) = true but length %d is out of range", id, len(id))
	}
}

// assertRequestIDHexOnly fails the test when an accepted request ID
// contains a character outside [0-9a-fA-F].
func assertRequestIDHexOnly(t *testing.T, id string) {
	t.Helper()
	for _, c := range id {
		if !isHexDigit(c) {
			t.Errorf("isValidRequestID(%q) = true but contains non-hex char %q", id, c)
		}
	}
}

// isHexDigit reports whether r is a single ASCII hexadecimal digit.
func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
