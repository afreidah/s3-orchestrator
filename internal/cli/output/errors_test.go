// -------------------------------------------------------------------------------
// CLI Output - Write-Error Path Tests
//
// Author: Alex Freidah
//
// Drives the renderers against a writer that always fails so the error-return
// branches (which a normal bytes.Buffer never triggers) are exercised.
// -------------------------------------------------------------------------------

package output

import (
	"errors"
	"testing"
)

// errWriter fails every Write, simulating a broken stdout (closed pipe, full
// disk) so the renderers' error returns are covered.
type errWriter struct{}

var errBroken = errors.New("broken writer")

func (errWriter) Write(_ []byte) (int, error) { return 0, errBroken }

// countingErrWriter succeeds until the failAt-th write, then fails. It reaches
// the deeper error branches that an always-failing writer can never get past.
type countingErrWriter struct {
	failAt int
	n      int
}

func (c *countingErrWriter) Write(p []byte) (int, error) {
	c.n++
	if c.n >= c.failAt {
		return 0, errBroken
	}
	return len(p), nil
}

func TestRenderValue_TopLevelScalarWriteError(t *testing.T) {
	t.Parallel()
	if err := RenderValue(errWriter{}, []byte(`"x"`)); err == nil {
		t.Error("expected write error on top-level scalar, got nil")
	}
}

func TestRenderValue_NestedWriteErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		raw    string
		failAt int
	}{
		{"map nested header", `{"outer":{"inner":1}}`, 1},
		{"map nested recursion", `{"outer":{"inner":1}}`, 2},
		{"slice object bullet", `[{"x":1}]`, 1},
		{"slice object recursion", `[{"x":1}]`, 2},
		{"slice scalar bullet", `["a"]`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &countingErrWriter{failAt: tc.failAt}
			if err := RenderValue(w, []byte(tc.raw)); err == nil {
				t.Errorf("expected write error at write %d, got nil", tc.failAt)
			}
		})
	}
}

func TestPrettyJSON_WriteError(t *testing.T) {
	t.Parallel()
	if err := PrettyJSON(errWriter{}, []byte(`{"a":1}`)); err == nil {
		t.Error("PrettyJSON with valid JSON: expected write error, got nil")
	}
	if err := PrettyJSON(errWriter{}, []byte(`not json`)); err == nil {
		t.Error("PrettyJSON with invalid JSON: expected passthrough write error, got nil")
	}
}

func TestTable_WriteError(t *testing.T) {
	t.Parallel()
	// tabwriter buffers, so the failure surfaces at Flush rather than on the
	// per-line writes; Table must still return it.
	if err := Table(errWriter{}, []string{"A"}, [][]string{{"1"}}); err == nil {
		t.Error("Table: expected write/flush error, got nil")
	}
}
