// -------------------------------------------------------------------------------
// Admin CLI - Bulk Rewrite Flag Tests
//
// Author: Alex Freidah
//
// All four fleet-wide rewrite commands carry -max, which converts part of a
// fleet and stops. The flag is the whole reason a fleet-sized conversion can be
// spread across maintenance windows, so what these tests hold is that the
// requested cap actually reaches the server: a -max the CLI drops silently
// looks like a bounded run and converts the entire fleet.
//
// Every command is checked rather than one, because they share a single flag
// set and a regression that drops the value drops it for all four.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// bulkRewriteCommands are the four commands sharing the -max flag set.
var bulkRewriteCommands = []struct {
	cmd      string
	wantPath string
}{
	{"compress-existing", "/admin/api/compress-existing"},
	{"decompress-existing", "/admin/api/decompress-existing"},
	{"encrypt-existing", "/admin/api/encrypt-existing"},
	{"decrypt-existing", "/admin/api/decrypt-existing"},
}

// runBulkRewriteCmd drives one command against a stub server and reports the
// exit code alongside the path and raw query the server actually saw.
func runBulkRewriteCmd(t *testing.T, cmd string, args []string) (code int, path, query string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "complete"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code = Command(cmd, args, srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("%s exit = %d, want 0; stderr=%s", cmd, code, stderr.String())
	}
	return code, path, query
}

// TestBulkRewrite_MaxReachesTheServer verifies -max=N is sent as the max query
// parameter.
func TestBulkRewrite_MaxReachesTheServer(t *testing.T) {
	t.Parallel()
	for _, tc := range bulkRewriteCommands {
		t.Run(tc.cmd, func(t *testing.T) {
			t.Parallel()
			_, path, query := runBulkRewriteCmd(t, tc.cmd, []string{"-max", "250"})
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if query != "max=250" {
				t.Errorf("query = %q, want max=250", query)
			}
		})
	}
}

// TestBulkRewrite_NoMaxSendsNoQuery verifies the whole-fleet form posts a bare
// path. A cap of zero has to reach the server as an absent parameter rather
// than max=0, which the handler would have to special-case.
func TestBulkRewrite_NoMaxSendsNoQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range bulkRewriteCommands {
		t.Run(tc.cmd, func(t *testing.T) {
			t.Parallel()
			for _, args := range [][]string{nil, {"-max", "0"}, {"-max", "-5"}} {
				_, path, query := runBulkRewriteCmd(t, tc.cmd, args)
				if path != tc.wantPath {
					t.Errorf("args %v: path = %q, want %q", args, path, tc.wantPath)
				}
				if query != "" {
					t.Errorf("args %v: query = %q, want none; a non-positive cap means the whole fleet", args, query)
				}
			}
		})
	}
}

// TestBulkRewrite_RejectsBadFlags verifies an unparseable flag set exits 1
// without reaching the server. A typo'd cap must not fall through to an
// unbounded pass over the fleet.
func TestBulkRewrite_RejectsBadFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range bulkRewriteCommands {
		t.Run(tc.cmd, func(t *testing.T) {
			t.Parallel()
			for _, args := range [][]string{{"-nonexistent-flag"}, {"-max", "not-a-number"}} {
				var stdout, stderr bytes.Buffer
				code := Command(tc.cmd, args, "http://127.0.0.1:0", "tok", &stdout, &stderr)
				if code != 1 {
					t.Errorf("args %v: exit = %d, want 1", args, code)
				}
			}
		})
	}
}
