// -------------------------------------------------------------------------------
// Admin Target Resolution
//
// Author: Alex Freidah
//
// Resolves the admin API base address and token shared by every client of the
// admin API (the admin CLI and the TUI). Precedence is flag -> environment
// ($S3O_ADMIN_ADDR / $S3O_ADMIN_TOKEN) -> config file, loading the config only
// when a value is still missing, so a local binary can target a remote instance
// with no server config at all.
// -------------------------------------------------------------------------------

// Package admintarget resolves the admin API base address and token from the
// precedence flag -> environment -> config file.
package admintarget

import (
	"os"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// EnvAddr and EnvToken let a local binary target a remote instance without a
// server config; flags take precedence over both.
const (
	EnvAddr  = "S3O_ADMIN_ADDR"
	EnvToken = "S3O_ADMIN_TOKEN" //nolint:gosec // G101: env var name, not a credential
)

// Resolve determines the admin API base address and token using the precedence
// flag -> environment -> config. The config file is loaded (via loadCfg) only
// when either value is still missing.
func Resolve(addrFlag, tokenFlag string, loadCfg func() (*config.Config, error)) (string, string, error) {
	addr := firstNonEmpty(addrFlag, os.Getenv(EnvAddr))
	token := firstNonEmpty(tokenFlag, os.Getenv(EnvToken))
	if addr != "" && token != "" {
		return addr, token, nil
	}

	cfg, err := loadCfg()
	if err != nil {
		return "", "", err
	}
	if addr == "" {
		addr = cfg.Server.ListenAddr
	}
	if token == "" {
		token = firstNonEmpty(cfg.UI.AdminToken, cfg.UI.AdminKey)
	}
	return addr, token, nil
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
