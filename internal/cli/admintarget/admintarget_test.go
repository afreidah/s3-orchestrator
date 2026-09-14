// -------------------------------------------------------------------------------
// Admin Target Resolution Tests
//
// Author: Alex Freidah
//
// Covers the flag -> environment -> config precedence of Resolve and the
// firstNonEmpty helper.
//
// Resolve answers for the address alone. Credentials come from their own flags
// or environment variables and never from the server's config, so there is no
// credential precedence left to cover here.
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

// cfgLoader returns a loader yielding a Config with the given listen address,
// used to exercise Resolve's config-fallback path.
func cfgLoader(addr string) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		c := &config.Config{}
		c.Server.ListenAddr = addr
		return c, nil
	}
}

// mustNotLoad fails the test if the config loader is invoked - used to prove
// Resolve skips the config file when the address came from a flag or the
// environment.
func mustNotLoad(t *testing.T) func() (*config.Config, error) {
	return func() (*config.Config, error) {
		t.Helper()
		t.Fatal("config loader must not be called when the address is supplied")
		return nil, nil
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestResolve_FlagBeatsEnvAndConfig verifies the flag wins over both the
// environment and the config file, which is not even loaded.
func TestResolve_FlagBeatsEnvAndConfig(t *testing.T) {
	t.Setenv(EnvAddr, "env-addr")

	addr, err := Resolve("flag-addr", mustNotLoad(t))
	if err != nil || addr != "flag-addr" {
		t.Fatalf("addr = %q, err = %v; want flag-addr", addr, err)
	}
}

// TestResolve_EnvUsedWhenNoFlag verifies the environment answers when no flag
// is given, still without reading the config.
func TestResolve_EnvUsedWhenNoFlag(t *testing.T) {
	t.Setenv(EnvAddr, "env-addr")

	addr, err := Resolve("", mustNotLoad(t))
	if err != nil || addr != "env-addr" {
		t.Fatalf("addr = %q, err = %v; want env-addr", addr, err)
	}
}

// TestResolve_ConfigFallback verifies the config file supplies the address when
// neither the flag nor the environment does.
func TestResolve_ConfigFallback(t *testing.T) {
	t.Setenv(EnvAddr, "")

	addr, err := Resolve("", cfgLoader("cfg-addr"))
	if err != nil || addr != "cfg-addr" {
		t.Fatalf("addr = %q, err = %v; want cfg-addr", addr, err)
	}
}

// TestResolve_LoaderError verifies a config that cannot be read surfaces rather
// than resolving to an empty address the caller would have to interpret.
func TestResolve_LoaderError(t *testing.T) {
	t.Setenv(EnvAddr, "")

	boom := errors.New("boom")
	if _, err := Resolve("", func() (*config.Config, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// TestFirstNonEmpty covers the precedence helper Resolve is built on.
func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []string
		want string
	}{
		{"first wins", []string{"a", "b"}, "a"},
		{"skips empties", []string{"", "", "c"}, "c"},
		{"all empty", []string{"", ""}, ""},
		{"none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstNonEmpty(tc.in...); got != tc.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
