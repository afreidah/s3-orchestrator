// -------------------------------------------------------------------------------
// Init CLI - Interactive Configuration Generator
//
// Author: Alex Freidah
//
// Generates a working configuration file by prompting for backend credentials,
// virtual bucket settings, and database driver. Defaults to SQLite for
// zero-dependency single-instance deployments.
// -------------------------------------------------------------------------------

package initcmd

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// Run is the CLI entry point. It parses the init flags, runs the interactive
// generator, and returns the process exit code.
func Run(args []string, in io.Reader, out, errOut io.Writer) int { // codecov:ignore -- interactive I/O wrapper
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Output path for the generated config file")
	_ = fs.Parse(args)

	if err := RunInteractive(*configPath, in, out); err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	return 0
}

// RunInteractive drives the interactive config generation flow. Exposed for
// tests that want to feed canned input through the prompter.
func RunInteractive(configPath string, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	if confirmed, err := confirmOverwrite(configPath, scanner, out); !confirmed || err != nil {
		return err
	}

	var params Params
	promptDatabase(scanner, out, &params)
	promptBackends(scanner, out, &params)
	promptBuckets(scanner, out, &params)

	output, err := GenerateConfig(&params)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}
	if err := validateGeneratedConfig(output); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(output), 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintf(out, "\nConfig written to %s\n", configPath)
	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintf(out, "  s3-orchestrator validate -config %s\n", configPath)
	fmt.Fprintf(out, "  s3-orchestrator -config %s\n", configPath)
	return nil
}

// confirmOverwrite prompts when the config file already exists. Returns
// (true, nil) if the caller should proceed, (false, nil) if the user
// aborted, or a wrapped error for any stat failure other than "not found".
func confirmOverwrite(configPath string, scanner *bufio.Scanner, out io.Writer) (bool, error) {
	if _, err := os.Stat(configPath); err != nil {
		return true, nil //nolint:nilerr // missing file is the normal init case
	}
	fmt.Fprintf(out, "Config file %q already exists. Overwrite? [y/N] ", configPath)
	if !scanYesNo(scanner) {
		fmt.Fprintln(out, "Aborted.")
		return false, nil
	}
	return true, nil
}

// promptDatabase collects database driver + connection inputs.
func promptDatabase(scanner *bufio.Scanner, out io.Writer, params *Params) {
	fmt.Fprintln(out, "\n--- Database ---")
	params.Driver = scanDefault(scanner, out, "Database driver (sqlite/postgres) [sqlite]: ", "sqlite")

	if params.Driver == "postgres" {
		params.DBHost = scanDefault(scanner, out, "PostgreSQL host [localhost]: ", "localhost")
		params.DBPort = scanDefault(scanner, out, "PostgreSQL port [5432]: ", "5432")
		params.DBName = scanDefault(scanner, out, "Database name [s3_orchestrator]: ", "s3_orchestrator")
		params.DBUser = scanDefault(scanner, out, "Database user [s3_orchestrator]: ", "s3_orchestrator")
		params.DBPassword = scanDefault(scanner, out, "Database password: ", "")
		return
	}
	params.Driver = "sqlite"
	params.DBPath = scanDefault(scanner, out, "SQLite database path [s3-orchestrator.db]: ", "s3-orchestrator.db")
}

// promptBackends collects one or more backend entries until the user
// declines to add another.
func promptBackends(scanner *bufio.Scanner, out io.Writer, params *Params) {
	fmt.Fprintln(out, "\n--- Storage Backend ---")
	for {
		params.Backends = append(params.Backends, promptSingleBackend(scanner, out))
		fmt.Fprint(out, "\nAdd another backend? [y/N] ")
		if !scanYesNo(scanner) {
			return
		}
	}
}

// promptSingleBackend collects the fields for one backend entry.
func promptSingleBackend(scanner *bufio.Scanner, out io.Writer) Backend {
	var be Backend
	be.Name = scanRequired(scanner, out, "Backend name: ")
	be.Endpoint = scanRequired(scanner, out, "S3 endpoint URL: ")
	be.Bucket = scanRequired(scanner, out, "S3 bucket name: ")
	be.AccessKeyID = scanRequired(scanner, out, "Access key ID: ")
	be.SecretAccessKey = scanRequired(scanner, out, "Secret access key: ")
	be.ForcePathStyle = strings.EqualFold(scanDefault(scanner, out, "Force path style? (yes for MinIO/OCI) [no]: ", "no"), "yes")

	fmt.Fprintln(out, "\nQuotas and limits (0 = unlimited):")
	be.QuotaBytes = scanDefault(scanner, out, "  Storage quota bytes [0]: ", "0")
	be.APIRequestLimit = scanDefault(scanner, out, "  Monthly API request limit [0]: ", "0")
	be.EgressByteLimit = scanDefault(scanner, out, "  Monthly egress byte limit [0]: ", "0")
	be.IngressByteLimit = scanDefault(scanner, out, "  Monthly ingress byte limit [0]: ", "0")
	return be
}

// promptBuckets collects one or more virtual-bucket entries.
func promptBuckets(scanner *bufio.Scanner, out io.Writer, params *Params) {
	fmt.Fprintln(out, "\n--- Virtual Bucket ---")
	for {
		var bkt Bucket
		bkt.Name = scanRequired(scanner, out, "Bucket name: ")
		bkt.AccessKeyID = scanRequired(scanner, out, "Client access key ID: ")
		bkt.SecretAccessKey = scanRequired(scanner, out, "Client secret access key: ")
		params.Buckets = append(params.Buckets, bkt)
		fmt.Fprint(out, "Add another bucket? [y/N] ")
		if !scanYesNo(scanner) {
			return
		}
	}
}

// validateGeneratedConfig writes the generated YAML to a temp file and
// runs the real LoadConfig over it to catch any defects before we
// overwrite the user's existing config.
func validateGeneratedConfig(yaml string) error {
	tmpFile, err := os.CreateTemp("", "s3orch-init-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(yaml); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	tmpFile.Close()

	if _, err := config.LoadConfig(tmpPath); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Params holds the collected user inputs for config generation.
type Params struct {
	Driver     string
	DBPath     string
	DBHost     string
	DBPort     string
	DBName     string
	DBUser     string
	DBPassword string
	Backends   []Backend
	Buckets    []Bucket
}

// Backend holds a single backend's configuration inputs.
type Backend struct {
	Name             string
	Endpoint         string
	Bucket           string
	AccessKeyID      string
	SecretAccessKey  string
	ForcePathStyle   bool
	QuotaBytes       string
	APIRequestLimit  string
	EgressByteLimit  string
	IngressByteLimit string
}

// Bucket holds a single virtual bucket's configuration inputs.
type Bucket struct {
	Name            string
	AccessKeyID     string
	SecretAccessKey string
}

// -------------------------------------------------------------------------
// CONFIG GENERATION
// -------------------------------------------------------------------------

// configTemplate is the YAML scaffold used by `s3-orchestrator init`
// to write a starter config.yaml. Fields are templated against the
// answers the interactive prompt collected, with engine-specific
// branches for sqlite vs postgres so the produced file is immediately
// runnable without manual edits.
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

// GenerateConfig renders the config template with the given parameters.
func GenerateConfig(params *Params) (string, error) {
	var buf strings.Builder
	if err := configTemplate.Execute(&buf, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// -------------------------------------------------------------------------
// INPUT HELPERS
// -------------------------------------------------------------------------

// scanDefault prints prompt and reads a line from the scanner, returning
// defaultVal if it is empty.
func scanDefault(scanner *bufio.Scanner, out io.Writer, prompt, defaultVal string) string {
	fmt.Fprint(out, prompt)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text != "" {
			return text
		}
	}
	return defaultVal
}

// scanRequired prints prompt and reads a line from the scanner, prompting
// again until it is non-empty.
func scanRequired(scanner *bufio.Scanner, out io.Writer, prompt string) string {
	for {
		fmt.Fprint(out, prompt)
		if scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text != "" {
				return text
			}
		}
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
