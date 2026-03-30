// -------------------------------------------------------------------------------
// Init Command Tests - Config Generation Validation
//
// Author: Alex Freidah
//
// Tests for the config generation logic used by the init subcommand. Verifies
// generated YAML round-trips through config.LoadConfig successfully for both
// SQLite and PostgreSQL driver configurations.
// -------------------------------------------------------------------------------

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestGenerateConfig_SQLite(t *testing.T) {
	params := initParams{
		Driver: "sqlite",
		DBPath: "test.db",
		Backends: []initBackend{
			{
				Name:             "minio",
				Endpoint:         "http://localhost:9000",
				Bucket:           "data",
				AccessKeyID:      "AKID",
				SecretAccessKey:  "SECRET",
				ForcePathStyle:   true,
				QuotaBytes:       "0",
				APIRequestLimit:  "0",
				EgressByteLimit:  "0",
				IngressByteLimit: "0",
			},
		},
		Buckets: []initBucket{
			{Name: "photos", AccessKeyID: "CLIENT_AK", SecretAccessKey: "CLIENT_SK"},
		},
	}

	output, err := generateConfig(&params)
	if err != nil {
		t.Fatalf("generateConfig: %v", err)
	}

	if !strings.Contains(output, "driver: sqlite") {
		t.Error("expected driver: sqlite in output")
	}
	if !strings.Contains(output, `path: "test.db"`) {
		t.Error("expected path in output")
	}

	// Round-trip through config loader
	tmpFile, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString(output); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := config.LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig on generated YAML: %v", err)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("driver = %q, want sqlite", cfg.Database.Driver)
	}
	if len(cfg.Backends) != 1 {
		t.Errorf("backends = %d, want 1", len(cfg.Backends))
	}
	if len(cfg.Buckets) != 1 {
		t.Errorf("buckets = %d, want 1", len(cfg.Buckets))
	}
}

func TestGenerateConfig_Postgres(t *testing.T) {
	params := initParams{
		Driver:     "postgres",
		DBHost:     "db.example.com",
		DBPort:     "5432",
		DBName:     "s3orch",
		DBUser:     "admin",
		DBPassword: "secret",
		Backends: []initBackend{
			{
				Name:             "aws",
				Endpoint:         "https://s3.us-east-1.amazonaws.com",
				Bucket:           "my-bucket",
				AccessKeyID:      "AKIA",
				SecretAccessKey:  "SECRET",
				ForcePathStyle:   false,
				QuotaBytes:       "10737418240",
				APIRequestLimit:  "50000",
				EgressByteLimit:  "10737418240",
				IngressByteLimit: "0",
			},
		},
		Buckets: []initBucket{
			{Name: "app", AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
	}

	output, err := generateConfig(&params)
	if err != nil {
		t.Fatalf("generateConfig: %v", err)
	}

	if !strings.Contains(output, "driver: postgres") {
		t.Error("expected driver: postgres in output")
	}
	if !strings.Contains(output, "api_request_limit: 50000") {
		t.Error("expected api_request_limit in output")
	}

	tmpFile, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString(output); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := config.LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig on generated YAML: %v", err)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("driver = %q, want postgres", cfg.Database.Driver)
	}
	if cfg.Backends[0].APIRequestLimit != 50000 {
		t.Errorf("api_request_limit = %d, want 50000", cfg.Backends[0].APIRequestLimit)
	}
}
