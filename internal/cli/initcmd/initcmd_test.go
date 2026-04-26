// -------------------------------------------------------------------------------
// Init Command Tests - Config Generation Validation
//
// Author: Alex Freidah
//
// Tests for the config generation logic used by the init subcommand. Verifies
// generated YAML round-trips through config.LoadConfig successfully for both
// SQLite and PostgreSQL driver configurations.
// -------------------------------------------------------------------------------

package initcmd

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// canned returns a bufio.Scanner reading from the given lines, joined with
// newlines. Used to drive the prompt helpers without a real TTY.
func canned(lines ...string) *bufio.Scanner {
	return bufio.NewScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// TestScanDefault_ReturnsDefaultWhenEmpty covers both the empty-string and
// trimmed-whitespace cases of scanDefault.
func TestScanDefault_ReturnsDefaultWhenEmpty(t *testing.T) {
	tests := []struct {
		input    string
		fallback string
		want     string
	}{
		{"value\n", "fallback", "value"},
		{"\n", "fallback", "fallback"},
		{"   \n", "fallback", "fallback"},
		{"  trimmed  \n", "fallback", "trimmed"},
	}
	for _, tt := range tests {
		s := bufio.NewScanner(strings.NewReader(tt.input))
		if got := scanDefault(s, tt.fallback); got != tt.want {
			t.Errorf("scanDefault(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestScanRequired_LoopsUntilNonEmpty drives the re-prompt branch by
// supplying empty lines before a real value.
func TestScanRequired_LoopsUntilNonEmpty(t *testing.T) {
	s := canned("", "  ", "answer")
	var out bytes.Buffer
	got := scanRequired(s, &out, "prompt: ")
	if got != "answer" {
		t.Errorf("scanRequired = %q, want answer", got)
	}
	if !strings.Contains(out.String(), "prompt: ") {
		t.Errorf("out = %q, want re-prompt", out.String())
	}
}

// TestScanYesNo covers the y/yes/N/empty branches.
func TestScanYesNo(t *testing.T) {
	cases := map[string]bool{
		"y\n":    true,
		"Y\n":    true,
		"yes\n":  true,
		"YES\n":  true,
		"n\n":    false,
		"\n":     false,
		"junk\n": false,
	}
	for in, want := range cases {
		s := bufio.NewScanner(strings.NewReader(in))
		if got := scanYesNo(s); got != want {
			t.Errorf("scanYesNo(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestConfirmOverwrite_FileMissing covers the happy path where no existing
// config file is present and the prompt is skipped.
func TestConfirmOverwrite_FileMissing(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmOverwrite(filepath.Join(t.TempDir(), "absent.yaml"), canned(), &out)
	if err != nil {
		t.Fatalf("confirmOverwrite: %v", err)
	}
	if !ok {
		t.Error("expected proceed when file is absent")
	}
}

// TestConfirmOverwrite_ExistsAccept covers the y-branch where the user
// accepts the overwrite prompt.
func TestConfirmOverwrite_ExistsAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ok, err := confirmOverwrite(path, canned("y"), &out)
	if err != nil {
		t.Fatalf("confirmOverwrite: %v", err)
	}
	if !ok {
		t.Error("expected proceed when user answers yes")
	}
}

// TestConfirmOverwrite_ExistsAbort covers the n-branch.
func TestConfirmOverwrite_ExistsAbort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ok, err := confirmOverwrite(path, canned("n"), &out)
	if err != nil {
		t.Fatalf("confirmOverwrite: %v", err)
	}
	if ok {
		t.Error("expected abort when user answers no")
	}
	if !strings.Contains(out.String(), "Aborted") {
		t.Errorf("out = %q, want abort message", out.String())
	}
}

// TestPromptDatabase_Sqlite covers the default-path branch where the user
// accepts sqlite and supplies a custom DB path.
func TestPromptDatabase_Sqlite(t *testing.T) {
	var out bytes.Buffer
	var p Params
	promptDatabase(canned("sqlite", "/tmp/my.db"), &out, &p)
	if p.Driver != "sqlite" {
		t.Errorf("Driver = %q, want sqlite", p.Driver)
	}
	if p.DBPath != "/tmp/my.db" {
		t.Errorf("DBPath = %q, want /tmp/my.db", p.DBPath)
	}
}

// TestPromptDatabase_Postgres covers the postgres branch where every host /
// port / db / user / password field gets read.
func TestPromptDatabase_Postgres(t *testing.T) {
	var out bytes.Buffer
	var p Params
	promptDatabase(canned("postgres", "db.host", "5433", "myorg", "alex", "secret"), &out, &p)
	if p.Driver != "postgres" {
		t.Errorf("Driver = %q, want postgres", p.Driver)
	}
	if p.DBHost != "db.host" || p.DBPort != "5433" || p.DBName != "myorg" ||
		p.DBUser != "alex" || p.DBPassword != "secret" {
		t.Errorf("postgres fields not parsed correctly: %+v", p)
	}
}

// TestPromptBackends_Single drives one backend entry then declines to add
// another, exercising promptSingleBackend end-to-end.
func TestPromptBackends_Single(t *testing.T) {
	input := canned(
		"primary",                 // name
		"http://localhost:9000",   // endpoint
		"data",                    // bucket
		"AKID",                    // access key
		"SECRET",                  // secret
		"yes",                     // force path style
		"0", "0", "0", "0",        // quotas
		"n",                       // add another? no
	)
	var out bytes.Buffer
	var p Params
	promptBackends(input, &out, &p)
	if len(p.Backends) != 1 {
		t.Fatalf("backends = %d, want 1", len(p.Backends))
	}
	be := p.Backends[0]
	if be.Name != "primary" || be.Endpoint != "http://localhost:9000" || !be.ForcePathStyle {
		t.Errorf("backend not populated correctly: %+v", be)
	}
}

// TestPromptBuckets_Single drives one bucket entry.
func TestPromptBuckets_Single(t *testing.T) {
	input := canned("photos", "AK", "SK", "n")
	var out bytes.Buffer
	var p Params
	promptBuckets(input, &out, &p)
	if len(p.Buckets) != 1 || p.Buckets[0].Name != "photos" {
		t.Errorf("buckets = %+v, want one named photos", p.Buckets)
	}
}

// TestValidateGeneratedConfig_RoundTrips checks that a freshly rendered
// config validates through the real loader.
func TestValidateGeneratedConfig_RoundTrips(t *testing.T) {
	yaml, err := GenerateConfig(&Params{
		Driver: "sqlite",
		DBPath: "test.db",
		Backends: []Backend{{
			Name: "b1", Endpoint: "http://x:1", Bucket: "b",
			AccessKeyID: "ak", SecretAccessKey: "sk",
			QuotaBytes: "0", APIRequestLimit: "0", EgressByteLimit: "0", IngressByteLimit: "0",
		}},
		Buckets: []Bucket{{Name: "vb", AccessKeyID: "ak", SecretAccessKey: "sk"}},
	})
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	if err := validateGeneratedConfig(yaml); err != nil {
		t.Fatalf("validateGeneratedConfig: %v", err)
	}
}

// TestValidateGeneratedConfig_RejectsInvalid covers the loader-returns-error
// branch: an empty YAML fails the buckets/backends required fields.
func TestValidateGeneratedConfig_RejectsInvalid(t *testing.T) {
	if err := validateGeneratedConfig("server:\n  listen_addr: ':9000'\n"); err == nil {
		t.Fatal("expected error from validateGeneratedConfig on bad YAML")
	}
}

// TestRunInteractive_HappyPath drives the full interactive flow with canned
// input and verifies the resulting file loads cleanly.
func TestRunInteractive_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.NewReader(strings.Join([]string{
		"sqlite",
		"db.sqlite",
		"primary", "http://localhost:9000", "data", "AKID", "SECRET", "no",
		"0", "0", "0", "0",
		"n", // no more backends
		"vb1", "ck", "cs",
		"n", // no more buckets
	}, "\n") + "\n")
	var out bytes.Buffer
	if err := RunInteractive(path, input, &out); err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if _, err := config.LoadConfig(path); err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
}

// TestRunInteractive_UserAborts covers the abort-on-existing-file branch.
func TestRunInteractive_UserAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("existing: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunInteractive(path, strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	// File contents preserved
	got, _ := os.ReadFile(path)
	if string(got) != "existing: 1\n" {
		t.Errorf("file was overwritten: got %q", got)
	}
}

func TestGenerateConfig_SQLite(t *testing.T) {
	params := Params{
		Driver: "sqlite",
		DBPath: "test.db",
		Backends: []Backend{
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
		Buckets: []Bucket{
			{Name: "photos", AccessKeyID: "CLIENT_AK", SecretAccessKey: "CLIENT_SK"},
		},
	}

	output, err := GenerateConfig(&params)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	if !strings.Contains(output, "driver: sqlite") {
		t.Error("expected driver: sqlite in output")
	}
	if !strings.Contains(output, `path: "test.db"`) {
		t.Error("expected path in output")
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
	params := Params{
		Driver:     "postgres",
		DBHost:     "db.example.com",
		DBPort:     "5432",
		DBName:     "s3orch",
		DBUser:     "admin",
		DBPassword: "secret",
		Backends: []Backend{
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
		Buckets: []Bucket{
			{Name: "app", AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
	}

	output, err := GenerateConfig(&params)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
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