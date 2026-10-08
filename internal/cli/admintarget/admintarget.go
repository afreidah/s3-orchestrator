// -------------------------------------------------------------------------------
// Admin Target Resolution
//
// Author: Alex Freidah
//
// Resolves the admin API base address shared by every client of the admin API
// (the admin CLI and the TUI). Precedence is flag -> environment
// ($S3O_ADMIN_ADDR) -> config file, loading the config only when the address is
// still missing, so a local binary can target a remote instance with no server
// config at all.
//
// Credentials are deliberately not resolved here: they come from the caller's
// own flags or environment, never from the server's config file.
// -------------------------------------------------------------------------------

package admintarget

import (
	"os"

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

// Resolve determines the admin API base address using the precedence
// flag -> environment -> config. The config file is loaded (via loadCfg) only
// when the address is still missing. Credentials never come from the server
// config, which holds the deployment's own identity rather than the operator's.
func Resolve(addrFlag string, loadCfg func() (*config.Config, error)) (string, error) {
	if addr := firstNonEmpty(addrFlag, os.Getenv(EnvAddr)); addr != "" {
		return addr, nil
	}
	cfg, err := loadCfg()
	if err != nil {
		return "", err
	}
	return cfg.Server.ListenAddr, nil
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
