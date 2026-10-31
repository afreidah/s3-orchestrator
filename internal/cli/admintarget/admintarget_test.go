// -------------------------------------------------------------------------------
// Admin Target Resolution Tests
//
// Author: Alex Freidah
//
// Covers the flag -> environment -> config precedence of the address, the
// http default for a bare host:port, and the refusal of a missing keypair. The
// config-file fallback itself is driven end to end by the adminctl tests.
// -------------------------------------------------------------------------------

package admintarget

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// missingConfig names a config file that does not exist, so a resolution that
// reads it fails and one that succeeds proves it was never read.
const missingConfig = "/no/such/file.yaml"

// resolve registers the flags on a fresh set, parses args, and resolves.
func resolve(t *testing.T, args ...string) (Target, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := RegisterFlags(fs)
	if err := fs.Parse(append([]string{"-config", missingConfig}, args...)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f.Resolve()
}

// clearEnv empties the variables Resolve falls back to, so a developer's shell
// cannot satisfy a case meant to fail.
func clearEnv(t *testing.T) {
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvAccessKey, "")
	t.Setenv(EnvSecretKey, "")
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestResolve_FlagBeatsEnvAndConfig verifies the flag wins over both the
// environment and the config file, which is not even loaded, and that a bare
// host:port gets the http scheme.
func TestResolve_FlagBeatsEnvAndConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAddr, "env-addr")

	got, err := resolve(t, "-addr", "flag-addr:9000", "-access-key", "AK", "-secret-key", "SK")
	if err != nil || got != (Target{Addr: "http://flag-addr:9000", AccessKeyID: "AK", SecretKey: "SK"}) {
		t.Fatalf("target = %+v, err = %v", got, err)
	}
}

// TestResolve_EnvUsedWhenNoFlag verifies the environment answers for both the
// address and the keypair when no flag is given, still without the config.
func TestResolve_EnvUsedWhenNoFlag(t *testing.T) {
	t.Setenv(EnvAddr, "https://env-addr")
	t.Setenv(EnvAccessKey, "AK")
	t.Setenv(EnvSecretKey, "SK")

	got, err := resolve(t)
	if err != nil || got != (Target{Addr: "https://env-addr", AccessKeyID: "AK", SecretKey: "SK"}) {
		t.Fatalf("target = %+v, err = %v", got, err)
	}
}

// TestResolve_KeypairRequired verifies half a keypair is refused before any
// address is looked for.
func TestResolve_KeypairRequired(t *testing.T) {
	clearEnv(t)

	if _, err := resolve(t, "-addr", "host:9000", "-access-key", "AK"); err == nil ||
		!strings.Contains(err.Error(), "credential is required") {
		t.Fatalf("err = %v, want the credential refusal", err)
	}
}

// TestResolve_ConfigReadOnlyWhenAddressMissing verifies the config file is
// read when neither the flag nor the environment names an address, and that a
// file that cannot be read surfaces rather than resolving to no address.
func TestResolve_ConfigReadOnlyWhenAddressMissing(t *testing.T) {
	clearEnv(t)

	if _, err := resolve(t, "-access-key", "AK", "-secret-key", "SK"); err == nil {
		t.Fatal("an unreadable config resolved")
	}
}
