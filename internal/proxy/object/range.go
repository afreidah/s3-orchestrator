// -------------------------------------------------------------------------------
// Object Manager - HTTP Range Parsing
//
// Author: Alex Freidah
//
// Plaintext Range header parser used by GetObject for encrypted objects.
// Resolves suffix and open-ended forms against the known plaintext size,
// rejects RFC 7233 invariant violations (inverted ranges, first-byte
// beyond end), and clamps the upper bound to the last valid offset so
// downstream ciphertext-range translation never requests chunks past
// the actual object.
// -------------------------------------------------------------------------------

package object

import (
	"strconv"
	"strings"
)

// ParsePlaintextRange extracts the start and end byte offsets from an HTTP
// Range header value (e.g., "bytes=0-99"). Suffix ranges and open-ended
// ranges are resolved against plaintextSize.
func ParsePlaintextRange(rangeHeader string, plaintextSize int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false
	}
	spec := rangeHeader[len("bytes="):]
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}

	if parts[0] == "" {
		// Suffix range: bytes=-N
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = max(plaintextSize-n, 0)
		return start, plaintextSize - 1, true
	}

	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	// Reject ranges whose first-byte-pos is beyond the file. Applies to both
	// open-ended (bytes=N-) and explicit (bytes=N-M) forms.
	if start >= plaintextSize {
		return 0, 0, false
	}

	if parts[1] == "" {
		// Open-ended: bytes=N-
		return start, plaintextSize - 1, true
	}

	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	// Reject inverted ranges per RFC 7233 (last-byte-pos >= first-byte-pos).
	if end < start {
		return 0, 0, false
	}

	// Clamp end to the last valid byte offset to prevent CiphertextRange
	// from requesting chunks beyond the actual object.
	if end >= plaintextSize {
		end = plaintextSize - 1
	}

	return start, end, true
}
