// -------------------------------------------------------------------------------
// Admin CLI - object-delete Tests
//
// Author: Alex Freidah
//
// Covers the two deletion forms: a single key goes to the object route, a
// prefix goes to the collection route only with -confirm, and the refusals
// that must not reach the server at all.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

// TestCmdObjectDelete_PrefixNeedsConfirm verifies a prefix deletion without
// -confirm is refused before any request is sent.
func TestCmdObjectDelete_PrefixNeedsConfirm(t *testing.T) {
	t.Parallel()
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("object-delete", []string{"-prefix", "bucket/dir/"}, srv.URL, testCreds, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected a non-zero exit for -prefix without -confirm")
	}
	if hit {
		t.Error("a request reached the server without -confirm")
	}
	if !strings.Contains(stderr.String(), "-confirm") {
		t.Errorf("stderr = %q, want it to name -confirm", stderr.String())
	}
}

// TestCmdObjectDelete_PrefixWithConfirm verifies -prefix -confirm issues a
// DELETE against the collection route carrying the prefix as a query.
func TestCmdObjectDelete_PrefixWithConfirm(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotPrefix string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotPrefix = r.Method, r.URL.Path, r.URL.Query().Get("prefix")
		_, _ = w.Write([]byte(`{"deleted":2,"failed":0,"total":2}`))
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
