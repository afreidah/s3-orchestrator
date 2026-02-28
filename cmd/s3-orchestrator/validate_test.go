// -------------------------------------------------------------------------------
// Validate Subcommand Tests
//
// Author: Alex Freidah
//
// Tests for the validateConfig function covering valid configs, missing files,
// and invalid config content.
// -------------------------------------------------------------------------------

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
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
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := validateConfig(path, &buf); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "valid") {
		t.Errorf("expected output to contain 'valid', got: %s", output)
	}
	if !strings.Contains(output, "backends: 1") {
		t.Errorf("expected output to contain 'backends: 1', got: %s", output)
	}
	if !strings.Contains(output, "buckets:  1") {
		t.Errorf("expected output to contain 'buckets:  1', got: %s", output)
	}
	if !strings.Contains(output, "routing:  pack") {
		t.Errorf("expected output to contain 'routing:  pack', got: %s", output)
	}
}

func TestValidateConfig_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	err := validateConfig("/nonexistent/config.yaml", &buf)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateConfig_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Missing required fields
	content := `
server:
  listen_addr: ":9000"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := validateConfig(path, &buf)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
}
