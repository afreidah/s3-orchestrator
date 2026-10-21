// -------------------------------------------------------------------------------
// byterange - Tests
//
// Author: Alex Freidah
//
// Pins every range form Parse accepts and rejects, including the clamps that
// keep a resolved range inside the object, and the two header formats.
// -------------------------------------------------------------------------------

package byterange

import "testing"

// TestParse pins the resolved offsets for each accepted form and rejects the
// malformed, inverted, multi-range and past-the-end forms.
func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		header     string
		size       int64
		start, end int64
		ok         bool
	}{
		{name: "explicit", header: "bytes=10-19", size: 100, start: 10, end: 19, ok: true},
		{name: "exact fit", header: "bytes=0-99", size: 100, start: 0, end: 99, ok: true},
		{name: "end clamped", header: "bytes=0-200", size: 100, start: 0, end: 99, ok: true},
		{name: "open ended", header: "bytes=40-", size: 100, start: 40, end: 99, ok: true},
		{name: "suffix", header: "bytes=-10", size: 100, start: 90, end: 99, ok: true},
		{name: "suffix larger than object", header: "bytes=-1000", size: 100, start: 0, end: 99, ok: true},
		{name: "empty", header: "", size: 100},
		{name: "wrong unit", header: "items=0-9", size: 100},
		{name: "no dash", header: "bytes=10", size: 100},
		{name: "zero suffix", header: "bytes=-0", size: 100},
		{name: "inverted", header: "bytes=99-0", size: 100},
		{name: "start past end", header: "bytes=100-200", size: 100},
		{name: "open ended past end", header: "bytes=100-", size: 100},
		{name: "multi range", header: "bytes=0-9,20-29", size: 100},
		{name: "not a number", header: "bytes=abc-def", size: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end, ok := Parse(tt.header, tt.size)
			if ok != tt.ok || start != tt.start || end != tt.end {
				t.Errorf("Parse(%q, %d) = %d, %d, %v; want %d, %d, %v",
					tt.header, tt.size, start, end, ok, tt.start, tt.end, tt.ok)
			}
		})
	}
}

// TestHeaders pins the Range and Content-Range formats.
func TestHeaders(t *testing.T) {
	t.Parallel()
	if got := Header(5, 9); got != "bytes=5-9" {
		t.Errorf("Header = %q", got)
	}
	if got := ContentRange(5, 9, 100); got != "bytes 5-9/100" {
		t.Errorf("ContentRange = %q", got)
	}
}
