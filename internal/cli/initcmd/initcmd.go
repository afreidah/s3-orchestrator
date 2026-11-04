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
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/afreidah/s3-orchestrator/internal/cli/flagset"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// Run is the CLI entry point. It parses the init flags, runs the interactive
// generator, and returns the process exit code.
func Run(args []string, in io.Reader, out, errOut io.Writer) int { // codecov:ignore -- interactive I/O wrapper
	fs := flagset.New("init", errOut)
	configPath := fs.String("config", "config.yaml", "Output path for the generated config file")
	if exit, ok := flagset.Parse(fs, args); !ok {
		return exit
	}

	if err := RunInteractive(*configPath, in, out); err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	return 0
}

// RunInteractive drives the interactive config generation flow. Exposed for
// tests that want to feed canned input through the prompter.
func RunInteractive(configPath string, in io.Reader, out io.Writer) error {
	p := newPrompter(in, out)
	if confirmed, err := confirmOverwrite(configPath, p); !confirmed || err != nil {
		return err
	}

	var params Params
	promptDatabase(p, &params)
	promptBackends(p, &params)
	promptBuckets(p, &params)
	if p.err != nil {
		return p.err
	}

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
func confirmOverwrite(configPath string, p *prompter) (bool, error) {
	if _, err := os.Stat(configPath); err != nil {
		return true, nil //nolint:nilerr // missing file is the normal init case
	}
	if !p.yes(fmt.Sprintf("Config file %q already exists. Overwrite?", configPath)) {
		fmt.Fprintln(p.out, "Aborted.")
		return false, nil
	}
	return true, nil
}

// promptDatabase collects database driver + connection inputs.
func promptDatabase(p *prompter, params *Params) {
	fmt.Fprintln(p.out, "\n--- Database ---")
	params.Driver = p.ask("Database driver (sqlite/postgres)", "sqlite")

	if params.Driver == "postgres" {
		params.DBHost = p.ask("PostgreSQL host", "localhost")
		params.DBPort = p.ask("PostgreSQL port", "5432")
		params.DBName = p.ask("Database name", "s3_orchestrator")
		params.DBUser = p.ask("Database user", "s3_orchestrator")
		params.DBPassword = p.ask("Database password", "")
		return
	}
	params.Driver = "sqlite"
	params.DBPath = p.ask("SQLite database path", "s3-orchestrator.db")
}

// promptBackends collects one or more backend entries until the user
// declines to add another.
func promptBackends(p *prompter, params *Params) {
	fmt.Fprintln(p.out, "\n--- Storage Backend ---")
	for {
		params.Backends = append(params.Backends, promptSingleBackend(p))
		if !p.yes("\nAdd another backend?") {
			return
		}
	}
}

// promptSingleBackend collects the fields for one backend entry.
func promptSingleBackend(p *prompter) Backend {
	var be Backend
	be.Name = p.require("Backend name")
	be.Endpoint = p.require("S3 endpoint URL")
	be.Bucket = p.require("S3 bucket name")
	be.AccessKeyID = p.require("Access key ID")
	be.SecretAccessKey = p.require("Secret access key")
	be.ForcePathStyle = strings.EqualFold(p.ask("Force path style? (yes for MinIO/OCI)", "no"), "yes")

	fmt.Fprintln(p.out, "\nQuotas and limits (0 = unlimited):")
	be.QuotaBytes = p.ask("  Storage quota bytes", "0")
	be.APIRequestLimit = p.ask("  Monthly API request limit", "0")
	be.EgressByteLimit = p.ask("  Monthly egress byte limit", "0")
	be.IngressByteLimit = p.ask("  Monthly ingress byte limit", "0")
	return be
}

// promptBuckets collects one or more virtual-bucket entries.
func promptBuckets(p *prompter, params *Params) {
	fmt.Fprintln(p.out, "\n--- Virtual Bucket ---")
	for {
		var bkt Bucket
		bkt.Name = p.require("Bucket name")
		bkt.AccessKeyID = p.require("Client access key ID")
		bkt.SecretAccessKey = p.require("Client secret access key")
		params.Buckets = append(params.Buckets, bkt)
		if !p.yes("Add another bucket?") {
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

// errInputEnded is what a required answer that never came fails the run with.
var errInputEnded = errors.New("input ended before every required answer was given")

// prompter asks the operator one question at a time. err records input that
// ended before a required answer came, which later questions then skip.
type prompter struct {
	in  *bufio.Scanner
	out io.Writer
	err error
}

// newPrompter reads answers from in and writes questions to out.
func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewScanner(in), out: out}
}

// answer reads one trimmed line, reporting false when the input has ended.
func (p *prompter) answer() (string, bool) {
	if !p.in.Scan() {
		return "", false
	}
	return strings.TrimSpace(p.in.Text()), true
}

// ask prints label with its fallback, when it has one, and returns the answer
// or the fallback if it is empty.
func (p *prompter) ask(label, fallback string) string {
	if fallback == "" {
		fmt.Fprintf(p.out, "%s: ", label)
	} else {
		fmt.Fprintf(p.out, "%s [%s]: ", label, fallback)
	}
	text, _ := p.answer()
	return cmp.Or(text, fallback)
}

// require asks until the answer is non-empty. Input that ends first records
// errInputEnded and returns nothing.
func (p *prompter) require(label string) string {
	for p.err == nil {
		fmt.Fprintf(p.out, "%s: ", label)
		text, ok := p.answer()
		if !ok {
			p.err = errInputEnded
		}
		if text != "" {
			return text
		}
	}
	return ""
}

// yes asks a y/N question, defaulting to no.
func (p *prompter) yes(question string) bool {
	fmt.Fprintf(p.out, "%s [y/N] ", question)
	text, _ := p.answer()
	text = strings.ToLower(text)
	return text == "y" || text == "yes"
}
