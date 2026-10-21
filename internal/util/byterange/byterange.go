// -------------------------------------------------------------------------------
// byterange - HTTP Byte-Range Parsing and Formatting
//
// Author: Alex Freidah
//
// Resolves a single RFC 9110 byte range against an object's size, and formats
// the Range header sent to a backend and the Content-Range header returned with
// a partial response. Every read path speaks these three shapes, so they live
// here rather than being reformatted at each call site.
// -------------------------------------------------------------------------------

package byterange

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse resolves a Range header value such as "bytes=0-99" against an object
// of size bytes, returning the inclusive offsets. Suffix ("bytes=-N") and
// open-ended ("bytes=N-") forms are resolved against size, and an end past the
// object is clamped to its last byte.
//
// ok is false for a missing, malformed or multi-range header, an inverted
// range, and a range that starts at or past the end of the object. Callers
// treat that as RFC 9110 allows: serve the whole object, or answer 416.
func Parse(header string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found {
		return 0, 0, false
	}
	from, to, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}

	if from == "" {
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size - 1, true
	}

	start, err := strconv.ParseInt(from, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if to == "" {
		return start, size - 1, true
	}

	end, err = strconv.ParseInt(to, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, min(end, size-1), true
}

// Header formats the Range request header value for the inclusive offsets
// start through end.
func Header(start, end int64) string {
	return fmt.Sprintf("bytes=%d-%d", start, end)
}

// ContentRange formats the Content-Range response header value for the
// inclusive offsets start through end of an object of size bytes.
func ContentRange(start, end, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", start, end, size)
}
