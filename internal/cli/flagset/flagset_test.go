// -------------------------------------------------------------------------------
// CLI Flag Sets - Tests
//
// Author: Alex Freidah
//
// Pins the exit code each outcome stops with: help is a success, and a bad or
// missing flag is not.
// -------------------------------------------------------------------------------

package flagset

import (
	"bytes"
	"strings"
	"testing"
)

// TestParse covers each outcome a command acts on.
func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		exit int
		ok   bool
		want string
	}{
		{"valid", []string{"-backend", "b1"}, 0, true, ""},
		{"help", []string{"-h"}, 0, false, "Usage of sync"},
		{"unknown flag", []string{"-nope"}, 1, false, "flag provided but not defined"},
		{"missing required", nil, 1, false, "error: -backend is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			fs := New("sync", &out)
			fs.String("backend", "", "Backend name")
			exit, ok := Parse(fs, tc.args, "backend")
			if exit != tc.exit || ok != tc.ok {
				t.Errorf("Parse = %d, %v; want %d, %v", exit, ok, tc.exit, tc.ok)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.want)
			}
		})
	}
}
