// -------------------------------------------------------------------------------
// Admin CLI - Workers Command Tests
//
// Author: Alex Freidah
//
// Covers the argument handling of `admin workers run`: a missing or extra
// worker name is a usage error that sends nothing, and an unknown verb lists
// the verbs the command takes.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"strings"
	"testing"
)

// TestWorkers_RunNeedsOneName verifies `workers run` with no name, or with
// more than one, prints its usage and fails without contacting the server.
func TestWorkers_RunNeedsOneName(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"run"}, {"run", "a", "b"}} {
		var stdout, stderr bytes.Buffer
		code := Command("workers", args, "http://127.0.0.1:1", testCreds, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "workers run <name>") {
			t.Errorf("args %v: exit=%d stderr=%q, want the usage and exit 1", args, code, stderr.String())
		}
	}
}

// TestWorkers_UnknownVerb verifies an unknown verb fails and lists run.
func TestWorkers_UnknownVerb(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("workers", []string{"restart"}, "http://127.0.0.1:1", testCreds, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "run") {
		t.Errorf("exit=%d stderr=%q, want a failure listing the run verb", code, stderr.String())
	}
}
