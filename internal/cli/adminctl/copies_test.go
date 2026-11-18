// -------------------------------------------------------------------------------
// Admin CLI - Copy Listing Tests
//
// Author: Alex Freidah
//
// Covers the table the unhashed and unreadable listings render, and that a
// malformed body falls back rather than rendering a partial table.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const copyListBody = `{
  "total": 12,
  "copies": [
    {"key":"bucket/new.bin","backend":"b1","size_bytes":2048,"created_at":"2026-07-07T07:44:51Z","managed":true,"encrypted":false},
    {"key":"stray.txt","backend":"b2","size_bytes":1024,"created_at":"2026-07-07T07:44:51Z","managed":false,"encrypted":true}
  ]
}`

// TestUnhashed_RendersTableAndTotal asserts `unhashed` renders each copy with
// its management and encryption, and how many of the total it shows.
func TestUnhashed_RendersTableAndTotal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(copyListBody))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := Command("unhashed", nil, srv.URL, testCreds, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"bucket/new.bin", "stray.txt", "2.0 KiB", "true", "false", "Showing 2 of 12 copies."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderCopyList_RejectsBadJSON asserts a malformed body surfaces a decode
// error, which makes the client fall back to raw JSON.
func TestRenderCopyList_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	if err := renderCopyList(&bytes.Buffer{}, []byte("{not json")); err == nil {
		t.Error("renderCopyList: expected a decode error")
	}
}
