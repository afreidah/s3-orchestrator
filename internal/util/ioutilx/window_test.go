// -------------------------------------------------------------------------------
// ioutilx - Stream Window Tests
//
// Author: Alex Freidah
//
// Pins the window Window cuts and the error for a stream too short to reach
// its start.
// -------------------------------------------------------------------------------

package ioutilx

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestWindow pins the bytes each window yields, including one that runs past
// the end of the stream and one with nothing to skip.
func TestWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		skip, n int64
		want    string
	}{
		{name: "middle", skip: 2, n: 3, want: "234"},
		{name: "no skip", skip: 0, n: 4, want: "0123"},
		{name: "past the end", skip: 8, n: 5, want: "89"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := Window(strings.NewReader("0123456789"), tt.skip, tt.n)
			if err != nil {
				t.Fatalf("Window: %v", err)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != tt.want {
				t.Errorf("read %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestWindow_StartPastEnd reports a stream that ends before the window starts
// as an unexpected EOF.
func TestWindow_StartPastEnd(t *testing.T) {
	t.Parallel()
	_, err := Window(strings.NewReader("0123"), 10, 1)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}
