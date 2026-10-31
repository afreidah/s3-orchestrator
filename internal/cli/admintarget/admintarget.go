// -------------------------------------------------------------------------------
// Admin Target Resolution
//
// Author: Alex Freidah
//
// Resolves the admin API endpoint and keypair shared by every client of the
// admin API (the admin CLI and the TUI). The address resolves flag ->
// environment ($S3O_ADMIN_ADDR) -> config file, loading the config only when
// the address is still missing, so a local binary can target a remote instance
// with no server config at all.
//
// Credentials resolve flag -> environment only, never from the server's config
// file, which holds the deployment's own identity rather than the operator's.
// -------------------------------------------------------------------------------

package admintarget

import (
	"cmp"
	"errors"
	"flag"
	"os"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// EnvAddr lets a local binary target a remote instance without a server config;
// the flag takes precedence over it.
//
// EnvAccessKey and EnvSecretKey carry the keypair a client signs with; they
// need no config file.
const (
	EnvAddr = "S3O_ADMIN_ADDR"

	EnvAccessKey = "S3O_ACCESS_KEY_ID"
	EnvSecretKey = "S3O_SECRET_ACCESS_KEY" //nolint:gosec // G101: env var name, not a credential
)

// Target is the resolved admin endpoint and the keypair to reach it with.
type Target struct {
	Addr        string
	AccessKeyID string
	SecretKey   string
}

// Flags holds the values of the flags RegisterFlags adds.
type Flags struct {
	configPath *string
	addr       *string
	accessKey  *string
	secretKey  *string
}

// RegisterFlags adds the flags every admin API client takes to fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	return &Flags{
		configPath: fs.String("config", "config.yaml", "Path to config file (only loaded when -addr and its env var are unset)"),
		addr:       fs.String("addr", "", "Server address (overrides $S3O_ADMIN_ADDR and config)"),
		accessKey:  fs.String("access-key", "", "Access key ID to sign with (overrides $S3O_ACCESS_KEY_ID)"),
		secretKey:  fs.String("secret-key", "", "Secret access key to sign with (overrides $S3O_SECRET_ACCESS_KEY)"),
	}
}

// Resolve resolves the target from the parsed flags. A missing keypair or
// address is an error naming where to set it, and the address comes back with
// a scheme.
func (f *Flags) Resolve() (Target, error) {
	t := Target{
		AccessKeyID: cmp.Or(*f.accessKey, os.Getenv(EnvAccessKey)),
		SecretKey:   cmp.Or(*f.secretKey, os.Getenv(EnvSecretKey)),
	}
	if t.AccessKeyID == "" || t.SecretKey == "" {
		return Target{}, errors.New("a credential is required (set -access-key and -secret-key, " +
			"or $S3O_ACCESS_KEY_ID and $S3O_SECRET_ACCESS_KEY)")
	}
	t.Addr = cmp.Or(*f.addr, os.Getenv(EnvAddr))
	if t.Addr == "" {
		cfg, err := config.LoadConfig(*f.configPath)
		if err != nil {
			return Target{}, err
		}
		t.Addr = cfg.Server.ListenAddr
	}
	if t.Addr == "" {
		return Target{}, errors.New("server address required (set -addr, $S3O_ADMIN_ADDR, or server.listen_addr in config)")
	}
	// A bare host:port defaults to http, because the common target is a local
	// instance reached over a loopback or a private network. An operator
	// pointing at a remote one supplies the scheme, and https is preserved
	// exactly because the prefix check passes it through untouched.
	if !strings.HasPrefix(t.Addr, "http") {
		t.Addr = "http://" + t.Addr //nolint:gosec // NOSONAR S5332: scheme default for an operator-supplied address
	}
	return t, nil
}
