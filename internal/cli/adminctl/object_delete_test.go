// -------------------------------------------------------------------------------
// Admin CLI - object-delete Tests
//
// Author: Alex Freidah
//
// Covers the two deletion forms: a single key goes to the object route, a
// prefix is previewed without -confirm and streamed with it, and the refusals
// that must not reach the server at all.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestCmdObjectDelete_KeyDeletesOneObject verifies -key issues a DELETE against
// the object route, with the key's slashes carried as the path.
func TestCmdObjectDelete_KeyDeletesOneObject(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"deleted":1}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("object-delete", []string{"-key", "bucket/dir/file.txt"}, srv.URL, testCreds, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/admin/api/objects/bucket/dir/file.txt" {
		t.Errorf("request = %s %s, want DELETE /admin/api/objects/bucket/dir/file.txt", gotMethod, gotPath)
	}
}

// TestCmdObjectDelete_PrefixPreviewsWithoutConfirm verifies a prefix deletion
// without -confirm only lists the prefix flat and states the count it found,
// as a floor when the page was truncated.
func TestCmdObjectDelete_PrefixPreviewsWithoutConfirm(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		page, want string
	}{
		"empty":     {`{"objects":[]}`, `Nothing under "bucket/dir/".`},
		"one page":  {`{"objects":[{"key":"bucket/dir/a"},{"key":"bucket/dir/b"}]}`, `2 objects under "bucket/dir/" would be deleted.`},
		"truncated": {`{"objects":[{"key":"bucket/dir/a"}],"truncated":true}`, `at least 1 objects under "bucket/dir/"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var gotMethod string
			var gotQuery url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotQuery = r.Method, r.URL.Query()
				_, _ = w.Write([]byte(tc.page))
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			if code := Command("object-delete", []string{"-prefix", "bucket/dir/"}, srv.URL, testCreds, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			if gotMethod != http.MethodGet || gotQuery.Get("prefix") != "bucket/dir/" || !gotQuery.Has("delimiter") || gotQuery.Get("delimiter") != "" {
				t.Errorf("request = %s ?%s, want a flat GET of the prefix", gotMethod, gotQuery.Encode())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}

// TestCmdObjectDelete_PrefixWithConfirm verifies -prefix -confirm asks for the
// progress stream on the collection route, carrying the prefix as a query.
func TestCmdObjectDelete_PrefixWithConfirm(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotPrefix, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotPrefix, gotAccept = r.Method, r.URL.Path, r.URL.Query().Get("prefix"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"event":"start","op":"delete-prefix"}
{"event":"step_end","message":"deleting bucket/dir/a","outcome":"ok"}
{"event":"result","outcome":"ok","processed":1,"message":"deleted 1, failed 0"}
`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("object-delete", []string{"-prefix", "bucket/dir/", "-confirm"}, srv.URL, testCreds, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/admin/api/objects" || gotPrefix != "bucket/dir/" {
		t.Errorf("request = %s %s prefix=%q, want DELETE /admin/api/objects prefix=bucket/dir/", gotMethod, gotPath, gotPrefix)
	}
	if !strings.Contains(gotAccept, "application/x-ndjson") {
		t.Errorf("Accept = %q, want the progress stream", gotAccept)
	}
	if !strings.Contains(stdout.String(), "deleting bucket/dir/a") {
		t.Errorf("stdout = %q, want a line per key", stdout.String())
	}
}

// TestCmdObjectDelete_KeyAndPrefixConflict verifies the two forms are refused
// together, since they would delete different sets of objects.
func TestCmdObjectDelete_KeyAndPrefixConflict(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("object-delete", []string{"-key", "bucket/k", "-prefix", "bucket/"}, "http://unused", testCreds, &stdout, &stderr)
	if code == 0 {
		t.Error("expected a non-zero exit for -key with -prefix")
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want it to explain the conflict", stderr.String())
	}
}

// TestCmdObjectDelete_RequiresTarget verifies a call naming neither form is
// refused rather than deleting nothing silently or the whole namespace.
func TestCmdObjectDelete_RequiresTarget(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("object-delete", nil, "http://unused", testCreds, &stdout, &stderr)
	if code == 0 {
		t.Error("expected a non-zero exit without -key or -prefix")
	}
	if !strings.Contains(stderr.String(), "-key or -prefix") {
		t.Errorf("stderr = %q, want it to name the required flags", stderr.String())
	}
}
