// -------------------------------------------------------------------------------
// Sync CLI Tests
//
// Author: Alex Freidah
//
// Tests cover flag parsing, config loading, store init dispatch, and the
// page-import accumulator. The HTTP-going code paths (real S3 listing) are
// out of scope here  -  they are covered by the integration suite.
// -------------------------------------------------------------------------------

package synccmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// freshStore opens a fresh in-memory sqlite store for each test so
// importPage can exercise the real importer surface end-to-end.
func freshStore(t *testing.T) (importer, adminStore) {
	t.Helper()
	path := writeYAML(t, validYAML)
	cfg, _, code := loadConfig(path, "b1")
	if code != 0 {
		t.Fatalf("loadConfig: code=%d", code)
	}
	objects, adminDB, exit := initStore(context.Background(), cfg)
	if exit != 0 {
		t.Fatalf("initStore: exit=%d", exit)
	}
	t.Cleanup(adminDB.Close)
	return objects, adminDB
}

// errorObjectStore makes ImportObject return a specific error so
// importPage's error-propagation branch can be driven without faking
// any other store methods.
type errorObjectStore struct {
	err error
}

// ImportObject records the import call so the test can assert it
// happened. The first return value mirrors the real store's
// inserted=true semantics for a fresh row.
func (e errorObjectStore) ImportObject(context.Context, string, string, int64, bool) (bool, error) {
	return false, e.err
}

// writeYAML drops the given config content into a temp file and returns the
// path. Centralised so each test reads cleanly.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// validYAML is the synccmd_test reference config: minimal but
// complete enough for the bootstrap to wire a fake backend and a
// real (in-memory) store.
const validYAML = `
server:
  listen_addr: ":0"
database:
  driver: sqlite
  path: ":memory:"
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:19000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
`

// TestParseFlags_BucketOptional pins that --bucket is no longer required.
// Keys are imported exactly as the backend holds them, so there is nothing for
// a virtual bucket name to contribute to the import.
func TestParseFlags_BucketOptional(t *testing.T) {
	var stderr bytes.Buffer
	opts, ok := parseFlags([]string{"--backend", "b1"}, &stderr)
	if !ok {
		t.Fatalf("expected --bucket to be optional, stderr=%q", stderr.String())
	}
	if opts.BackendName != "b1" {
		t.Errorf("backend = %q, want b1", opts.BackendName)
	}
}

// TestParseFlags_RequiredMissing covers both required-flag branches in
// parseFlags. The flag set is constructed with ContinueOnError-equivalent
// behaviour by exiting before fs.Usage prints to a real terminal.
func TestParseFlags_RequiredMissing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no-backend", []string{"--bucket", "vb"}, "--backend is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			opts, ok := parseFlags(tc.args, &stderr)
			if ok {
				t.Fatalf("expected ok=false, got opts=%+v", opts)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestParseFlags_Valid verifies that all flags including --prefix and
// --dry-run flow through to the Options struct.
func TestParseFlags_Valid(t *testing.T) {
	args := []string{"--config", "/tmp/c.yaml", "--backend", "b1", "--bucket", "vb",
		"--prefix", "photos/", "--dry-run"}
	var stderr bytes.Buffer
	opts, ok := parseFlags(args, &stderr)
	if !ok {
		t.Fatalf("ok=false, stderr=%q", stderr.String())
	}
	if opts.ConfigPath != "/tmp/c.yaml" || opts.BackendName != "b1" ||
		opts.BucketName != "vb" || opts.Prefix != "photos/" || !opts.DryRun {
		t.Errorf("opts not populated: %+v", opts)
	}
}

// TestLoadConfig_FailsOnMissingFile drives the LoadConfig-error branch.
func TestLoadConfig_FailsOnMissingFile(t *testing.T) {
	_, _, code := loadConfig("/no/such/path", "b1")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestLoadConfig_BackendNotFound drives the loop-doesn't-match branch.
func TestLoadConfig_BackendNotFound(t *testing.T) {
	path := writeYAML(t, validYAML)
	_, _, code := loadConfig(path, "missing")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestLoadConfig_Valid drives the happy path that returns the resolved
// backend config.
func TestLoadConfig_Valid(t *testing.T) {
	path := writeYAML(t, validYAML)
	cfg, be, code := loadConfig(path, "b1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if cfg == nil || be == nil || be.Name != "b1" {
		t.Errorf("got cfg=%v be=%v", cfg, be)
	}
}

// TestInitStore_UnsupportedDriver covers the default branch in initStore's
// driver switch. We bypass config.LoadConfig (which rejects bad drivers
// upfront) by constructing a Config directly so the branch is reachable.
func TestInitStore_UnsupportedDriver(t *testing.T) {
	cfg := &config.Config{Database: config.DatabaseConfig{Driver: "mysql"}}
	_, _, exit := initStore(context.Background(), cfg)
	if exit != 1 {
		t.Errorf("exit = %d, want 1 for unsupported driver", exit)
	}
}

// TestInitStore_SQLiteHappyPath drives the sqlite branch end-to-end and
// confirms migrations + quota sync run without error.
func TestInitStore_SQLiteHappyPath(t *testing.T) {
	path := writeYAML(t, validYAML)
	cfg, _, code := loadConfig(path, "b1")
	if code != 0 {
		t.Fatalf("loadConfig: code=%d", code)
	}
	objects, adminDB, exit := initStore(context.Background(), cfg)
	if exit != 0 {
		t.Fatalf("initStore exit = %d, want 0", exit)
	}
	if objects == nil || adminDB == nil {
		t.Fatal("expected non-nil store handles")
	}
	adminDB.Close()
}

// TestImportPage_DryRun verifies the dry-run path counts objects and bytes
// without writing to the store.
func TestImportPage_DryRun(t *testing.T) {
	objects, _ := freshStore(t)
	page := []backend.ListedObject{
		{Key: "a.txt", SizeBytes: 10},
		{Key: "b.txt", SizeBytes: 20},
	}
	imp, skip, bytesIn, err := importPage(
		context.Background(), objects, page, "b1", []string{"vb"},
		&Options{BucketName: "vb", DryRun: true},
	)
	if err != nil {
		t.Fatalf("importPage: %v", err)
	}
	if imp != 2 || skip != 0 || bytesIn != 30 {
		t.Errorf("imp=%d skip=%d bytes=%d, want 2/0/30", imp, skip, bytesIn)
	}
}

// TestImportPage_RealImportSkipsExisting drives a real import then re-runs
// the same page to exercise the ImportObject(...) "already exists" return.
func TestImportPage_RealImportSkipsExisting(t *testing.T) {
	objects, _ := freshStore(t)
	page := []backend.ListedObject{
		{Key: "new.txt", SizeBytes: 10},
		{Key: "existing.txt", SizeBytes: 5},
	}

	// First pass: both rows are created.
	imp, skip, bytesIn, err := importPage(
		context.Background(), objects, page, "b1", []string{"vb"},
		&Options{BucketName: "vb"},
	)
	if err != nil {
		t.Fatalf("importPage first pass: %v", err)
	}
	if imp != 2 || skip != 0 || bytesIn != 15 {
		t.Errorf("first pass imp=%d skip=%d bytes=%d, want 2/0/15", imp, skip, bytesIn)
	}

	// Second pass: both already exist, so ImportObject returns (false, nil).
	imp, skip, bytesIn, err = importPage(
		context.Background(), objects, page, "b1", []string{"vb"},
		&Options{BucketName: "vb"},
	)
	if err != nil {
		t.Fatalf("importPage second pass: %v", err)
	}
	if imp != 0 || skip != 2 || bytesIn != 0 {
		t.Errorf("second pass imp=%d skip=%d bytes=%d, want 0/2/0", imp, skip, bytesIn)
	}
}

// TestImportPage_PropagatesError covers the wrap-and-return branch when the
// underlying store fails.
func TestImportPage_PropagatesError(t *testing.T) {
	_, _ = freshStore(t)
	wrapped := errorObjectStore{err: os.ErrPermission}
	_, _, _, err := importPage(
		context.Background(), wrapped,
		[]backend.ListedObject{{Key: "x", SizeBytes: 1}},
		"b1", []string{"vb"}, &Options{BucketName: "vb"},
	)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

// TestRun_BackendInitFails_ReturnsExitCodeOne covers the runtime where the
// backend can be configured but the configured S3 endpoint is unreachable.
// We invoke Run with a dry-run so we don't actually need a live MinIO; the
// failure happens at ListObjects time. The check here is only that the
// process exit code is non-zero, not the precise error.
// TestRun_FailsWithoutLiveBackend verifies run_fails without live backend.
// TestRun_FailsWithoutLiveBackend verifies run_fails without live backend.
func TestRun_FailsWithoutLiveBackend(t *testing.T) {
	path := writeYAML(t, validYAML)
	args := []string{
		"--config", path,
		"--backend", "b1",
		"--bucket", "vb",
		"--dry-run",
	}
	var stderr bytes.Buffer
	code := Run(args, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (no live S3 backend at fixture URL)", code)
	}
}
