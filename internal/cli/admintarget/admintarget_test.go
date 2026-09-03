// -------------------------------------------------------------------------------
// Admin Target Resolution Tests
//
// Author: Alex Freidah
//
// Covers the flag -> environment -> config precedence of Resolve and the
// firstNonEmpty helper.
// -------------------------------------------------------------------------------

package admintarget

import (
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// cfgLoader returns a loader that yields a Config with the given address and
// admin credentials, used to exercise Resolve's config-fallback path.
func cfgLoader(addr, adminToken, adminKey string) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		c := &config.Config{}
		c.Server.ListenAddr = addr
		c.UI.AdminToken = adminToken
		c.UI.AdminKey = adminKey
		return c, nil
	}
}

// mustNotLoad fails the test if the config loader is invoked - used to prove
// Resolve skips the config file when addr and token come from flags/env.
func mustNotLoad(t *testing.T) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		t.Helper()
		t.Fatal("config loader must not be called when addr and token are supplied")
		return nil, nil
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestResolve_FlagsBeatEnvAndConfig verifies flags win over both the
// environment and the config file (which is not even loaded).
func TestResolve_FlagsBeatEnvAndConfig(t *testing.T) {
	t.Setenv(EnvAddr, "env-addr")
	t.Setenv(EnvToken, "env-tok")
	addr, tok, err := Resolve("flag-addr", "flag-tok", mustNotLoad(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "flag-addr" || tok != "flag-tok" {
		t.Errorf("got (%q,%q), want (flag-addr, flag-tok)", addr, tok)
	}
}

// TestResolve_EnvUsedWhenNoFlags verifies env vars are used when no flags are
// set, again without touching the config file.
func TestResolve_EnvUsedWhenNoFlags(t *testing.T) {
	t.Setenv(EnvAddr, "env-addr")
	t.Setenv(EnvToken, "env-tok")
	addr, tok, err := Resolve("", "", mustNotLoad(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "env-addr" || tok != "env-tok" {
		t.Errorf("got (%q,%q), want (env-addr, env-tok)", addr, tok)
	}
}

// TestResolve_ConfigFallback verifies that with neither flags nor env, both
// values come from config, and admin_key is used when admin_token is empty.
func TestResolve_ConfigFallback(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvToken, "")
	addr, tok, err := Resolve("", "", cfgLoader("cfg-addr", "", "cfg-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "cfg-addr" || tok != "cfg-key" {
		t.Errorf("got (%q,%q), want (cfg-addr, cfg-key)", addr, tok)
	}
}

// TestResolve_AdminTokenPreferredOverKey verifies admin_token beats admin_key
// when both are present in config.
func TestResolve_AdminTokenPreferredOverKey(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvToken, "")
	_, tok, err := Resolve("", "", cfgLoader("a", "the-token", "the-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "the-token" {
		t.Errorf("token = %q, want the-token", tok)
	}
}

// TestResolve_FlagAddrConfigToken verifies the mixed case: address from a flag,
// token from config (config is loaded because the token is still missing).
func TestResolve_FlagAddrConfigToken(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvToken, "")
	addr, tok, err := Resolve("flag-addr", "", cfgLoader("cfg-addr", "", "cfg-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "flag-addr" || tok != "cfg-key" {
		t.Errorf("got (%q,%q), want (flag-addr, cfg-key)", addr, tok)
	}
}

// TestResolve_LoaderError verifies a config load failure surfaces verbatim.
func TestResolve_LoaderError(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvToken, "")
	sentinel := errors.New("bad config")
	_, _, err := Resolve("", "", func() (*config.Config, error) { return nil, sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestFirstNonEmpty verifies the first non-empty string wins.
func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want third", got)
	}
	if got := firstNonEmpty("first", "second"); got != "first" {
		t.Errorf("got %q, want first", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
