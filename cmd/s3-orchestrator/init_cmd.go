// -------------------------------------------------------------------------------
// Init Subcommand - Interactive Configuration Generator
//
// Author: Alex Freidah
//
// Generates a working configuration file by prompting for backend credentials,
// virtual bucket settings, and database driver. Defaults to SQLite for
// zero-dependency single-instance deployments.
// -------------------------------------------------------------------------------

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// runInit parses flags and runs the interactive config generator.
func runInit() { // codecov:ignore -- interactive I/O wrapper, logic tested via generateConfig
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Output path for the generated config file")
	_ = fs.Parse(os.Args[1:])

	if err := runInitInteractive(*configPath, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runInitInteractive drives the interactive config generation flow.
func runInitInteractive(configPath string, in *os.File, out *os.File) error {
	scanner := bufio.NewScanner(in)

	// Check for existing file
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(out, "Config file %q already exists. Overwrite? [y/N] ", configPath)
		if !scanYesNo(scanner) {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	var params initParams

	// --- Database driver ---
	fmt.Fprintln(out, "\n--- Database ---")
	fmt.Fprint(out, "Database driver (sqlite/postgres) [sqlite]: ")
	params.Driver = scanDefault(scanner, "sqlite")

	if params.Driver == "postgres" {
		fmt.Fprint(out, "PostgreSQL host [localhost]: ")
		params.DBHost = scanDefault(scanner, "localhost")
		fmt.Fprint(out, "PostgreSQL port [5432]: ")
		params.DBPort = scanDefault(scanner, "5432")
		fmt.Fprint(out, "Database name [s3_orchestrator]: ")
		params.DBName = scanDefault(scanner, "s3_orchestrator")
		fmt.Fprint(out, "Database user [s3_orchestrator]: ")
		params.DBUser = scanDefault(scanner, "s3_orchestrator")
		fmt.Fprint(out, "Database password: ")
		params.DBPassword = scanDefault(scanner, "")
	} else {
		params.Driver = "sqlite"
		fmt.Fprint(out, "SQLite database path [s3-orchestrator.db]: ")
		params.DBPath = scanDefault(scanner, "s3-orchestrator.db")
	}

	// --- Backend ---
	fmt.Fprintln(out, "\n--- Storage Backend ---")
	for {
		var be initBackend
		fmt.Fprint(out, "Backend name: ")
		be.Name = scanRequired(scanner, out, "Backend name: ")
		fmt.Fprint(out, "S3 endpoint URL: ")
		be.Endpoint = scanRequired(scanner, out, "S3 endpoint URL: ")
		fmt.Fprint(out, "S3 bucket name: ")
		be.Bucket = scanRequired(scanner, out, "S3 bucket name: ")
		fmt.Fprint(out, "Access key ID: ")
		be.AccessKeyID = scanRequired(scanner, out, "Access key ID: ")
		fmt.Fprint(out, "Secret access key: ")
		be.SecretAccessKey = scanRequired(scanner, out, "Secret access key: ")
		fmt.Fprint(out, "Force path style? (yes for MinIO/OCI) [no]: ")
		be.ForcePathStyle = strings.EqualFold(scanDefault(scanner, "no"), "yes")

		fmt.Fprintln(out, "\nQuotas and limits (0 = unlimited):")
		fmt.Fprint(out, "  Storage quota bytes [0]: ")
		be.QuotaBytes = scanDefault(scanner, "0")
		fmt.Fprint(out, "  Monthly API request limit [0]: ")
		be.APIRequestLimit = scanDefault(scanner, "0")
		fmt.Fprint(out, "  Monthly egress byte limit [0]: ")
		be.EgressByteLimit = scanDefault(scanner, "0")
		fmt.Fprint(out, "  Monthly ingress byte limit [0]: ")
		be.IngressByteLimit = scanDefault(scanner, "0")

		params.Backends = append(params.Backends, be)

		fmt.Fprint(out, "\nAdd another backend? [y/N] ")
		if !scanYesNo(scanner) {
			break
		}
	}

	// --- Virtual bucket ---
	fmt.Fprintln(out, "\n--- Virtual Bucket ---")
	for {
		var bkt initBucket
		fmt.Fprint(out, "Bucket name: ")
		bkt.Name = scanRequired(scanner, out, "Bucket name: ")
		fmt.Fprint(out, "Client access key ID: ")
		bkt.AccessKeyID = scanRequired(scanner, out, "Client access key ID: ")
		fmt.Fprint(out, "Client secret access key: ")
		bkt.SecretAccessKey = scanRequired(scanner, out, "Client secret access key: ")

		params.Buckets = append(params.Buckets, bkt)

		fmt.Fprint(out, "Add another bucket? [y/N] ")
		if !scanYesNo(scanner) {
			break
		}
	}

	// --- Generate and write ---
	output, err := generateConfig(params)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	// Validate before writing
	tmpFile, err := os.CreateTemp("", "s3orch-init-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(output); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	tmpFile.Close()

	if _, err := config.LoadConfig(tmpPath); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}

	if err := os.WriteFile(configPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintf(out, "\nConfig written to %s\n", configPath)
	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintf(out, "  s3-orchestrator validate -config %s\n", configPath)
	fmt.Fprintf(out, "  s3-orchestrator -config %s\n", configPath)
	return nil
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// initParams holds the collected user inputs for config generation.
type initParams struct {
	Driver     string
	DBPath     string
	DBHost     string
	DBPort     string
	DBName     string
	DBUser     string
	DBPassword string
	Backends   []initBackend
	Buckets    []initBucket
}

// initBackend holds a single backend's configuration inputs.
type initBackend struct {
	Name            string
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	QuotaBytes      string
	APIRequestLimit string
	EgressByteLimit string
	IngressByteLimit string
}

// initBucket holds a single virtual bucket's configuration inputs.
type initBucket struct {
	Name            string
	AccessKeyID     string
	SecretAccessKey string
}

// -------------------------------------------------------------------------
// CONFIG GENERATION
// -------------------------------------------------------------------------

var configTemplate = template.Must(template.New("config").Parse(`server:
  listen_addr: ":9000"

database:
{{- if eq .Driver "sqlite"}}
  driver: sqlite
  path: "{{.DBPath}}"
{{- else}}
  driver: postgres
  host: "{{.DBHost}}"
  port: {{.DBPort}}
  database: "{{.DBName}}"
  user: "{{.DBUser}}"
  password: "{{.DBPassword}}"
{{- end}}

routing_strategy: "pack"

backends:
{{- range .Backends}}
  - name: "{{.Name}}"
    endpoint: "{{.Endpoint}}"
    bucket: "{{.Bucket}}"
    access_key_id: "{{.AccessKeyID}}"
    secret_access_key: "{{.SecretAccessKey}}"
    force_path_style: {{.ForcePathStyle}}
    quota_bytes: {{.QuotaBytes}}
    api_request_limit: {{.APIRequestLimit}}
    egress_byte_limit: {{.EgressByteLimit}}
    ingress_byte_limit: {{.IngressByteLimit}}
{{- end}}

buckets:
{{- range .Buckets}}
  - name: "{{.Name}}"
    credentials:
      - access_key_id: "{{.AccessKeyID}}"
        secret_access_key: "{{.SecretAccessKey}}"
{{- end}}
`))

// generateConfig renders the config template with the given parameters.
func generateConfig(params initParams) (string, error) {
	var buf strings.Builder
	if err := configTemplate.Execute(&buf, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// -------------------------------------------------------------------------
// INPUT HELPERS
// -------------------------------------------------------------------------

// scanDefault reads a line from the scanner, returning defaultVal if empty.
func scanDefault(scanner *bufio.Scanner, defaultVal string) string {
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text != "" {
			return text
		}
	}
	return defaultVal
}

// scanRequired reads a line from the scanner, re-prompting until non-empty.
func scanRequired(scanner *bufio.Scanner, out *os.File, prompt string) string {
	for {
		if scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text != "" {
				return text
			}
		}
		fmt.Fprint(out, prompt)
	}
}

// scanYesNo reads a y/n response, defaulting to no.
func scanYesNo(scanner *bufio.Scanner) bool {
	if scanner.Scan() {
		text := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return text == "y" || text == "yes"
	}
	return false
}
