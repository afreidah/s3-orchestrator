// -------------------------------------------------------------------------------
// CountingWriter Tests
//
// Author: Alex Freidah
//
// Pins that the count covers only what the wrapped writer accepted, including
// a short write that fails partway.
// -------------------------------------------------------------------------------

package ioutilx

import (
	"bytes"
	"errors"
	"testing"
)

// shortWriter accepts at most limit bytes per call and then fails.
type shortWriter struct{ limit int }

// Write implements io.Writer.
func (s shortWriter) Write(p []byte) (int, error) {
	if len(p) > s.limit {
		return s.limit, errShort
	}
	return len(p), nil
}

// errShort is the failure shortWriter reports.
var errShort = errors.New("short write")

// TestCountingWriter_CountsAcceptedBytes checks the count sums successive
// writes and forwards the bytes unchanged.
func TestCountingWriter_CountsAcceptedBytes(t *testing.T) {
	t.Parallel()
	var dst bytes.Buffer
	c := &CountingWriter{W: &dst}
	_, _ = c.Write([]byte("hello "))
	_, _ = c.Write([]byte("world"))
	if c.N != 11 || dst.String() != "hello world" {
		t.Errorf("N = %d, dst = %q, want 11 and %q", c.N, dst.String(), "hello world")
	}
}

// TestCountingWriter_ShortWrite checks a failed write counts only what the
// wrapped writer took and returns its error.
func TestCountingWriter_ShortWrite(t *testing.T) {
	t.Parallel()
	c := &CountingWriter{W: shortWriter{limit: 3}}
	n, err := c.Write([]byte("abcdef"))
	if n != 3 || c.N != 3 || !errors.Is(err, errShort) {
		t.Errorf("n = %d, N = %d, err = %v, want 3, 3, errShort", n, c.N, err)
	}
}
