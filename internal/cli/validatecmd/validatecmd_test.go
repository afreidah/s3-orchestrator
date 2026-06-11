// -------------------------------------------------------------------------------
// Validate CLI Tests
//
// Author: Alex Freidah
//
// Tests for validate covering valid configs, missing files, and invalid config
// content, plus the Run entry point's exit codes.
// -------------------------------------------------------------------------------

package validatecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
server:
  listen_addr: ":9000"
database:
  host: localhost
  database: testdb
  user: testuser
  password: testpass
buckets:
  - name: test
    credentials:
      - access_key_id: ak
        secret_access_key: sk
backends:
  - name: b1
    endpoint: http://localhost:9000
    region: us-east-1
    bucket: bucket1
    access_key_id: ak
    secret_access_key: sk
`

// TestValidate_ValidFile asserts a well-formed config validates and the summary
// reports the parsed backend/bucket/routing values.
func TestValidate_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := validate(path, &buf); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	output := buf.String()
	for _, want := range []string{"valid", "backends: 1", "buckets:  1", "routing:  pack"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected output to contain %q, got: %s", want, output)
		}
	}
}

// TestValidate_MissingFile verifies a missing config file is an error.
func TestValidate_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	if err := validate("/nonexistent/config.yaml", &buf); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestValidate_InvalidConfig verifies a config missing required fields is an error.
func TestValidate_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen_addr: \":9000\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := validate(path, &buf); err == nil {
		t.Fatal("expected error for invalid config")
	}
}

// TestRun_ExitCodes covers the Run wrapper's exit codes for a valid and a
// missing config file.
func TestRun_ExitCodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"-config", path}, &stdout, &stderr); code != 0 {
		t.Errorf("Run exit = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if code := Run([]string{"-config", "/nonexistent/config.yaml"}, &stdout, &stderr); code != 1 {
		t.Errorf("Run exit = %d, want 1 on missing file", code)
	}
}
