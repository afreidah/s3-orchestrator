// -------------------------------------------------------------------------------
// Authentication Configuration Tests
//
// Author: Alex Freidah
//
// The root credential is the identity a deployment administers itself with, so
// a half-declared one is refused rather than silently producing a credential
// that cannot sign.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"testing"
)

// TestAuthConfig_RootCredentialIsAllOrNothing verifies both halves are required
// together. A key with no secret cannot sign and a secret with no key names
// nothing, so either alone is a mistake rather than a partial configuration.
func TestAuthConfig_RootCredentialIsAllOrNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		root    RootCredential
		wantErr bool
	}{
		{"neither", RootCredential{}, false},
		{"both", RootCredential{AccessKeyID: "AK", SecretAccessKey: "SK"}, false},
		{"key alone", RootCredential{AccessKeyID: "AK"}, true},
		{"secret alone", RootCredential{SecretAccessKey: "SK"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := AuthConfig{Root: tc.root}
			errs := a.setDefaultsAndValidate(false)
			if tc.wantErr && !errors.Is(errors.Join(errs...), ErrRootCredentialIncomplete) {
				t.Errorf("errs = %v, want ErrRootCredentialIncomplete", errs)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("errs = %v, want none", errs)
			}
		})
	}
}

// TestAuthConfig_RootRequiredWhereSomethingNeedsIt verifies a deployment that
// enables the dashboard must declare a root credential.
//
// It is the only way in once the separate dashboard login is gone, so a config
// without one would boot with nothing able to provision the first user.
func TestAuthConfig_RootRequiredWhereSomethingNeedsIt(t *testing.T) {
	t.Parallel()

	var none AuthConfig
	if errs := none.setDefaultsAndValidate(false); len(errs) != 0 {
		t.Errorf("errs = %v, want none when nothing requires a root credential", errs)
	}
	if errs := none.setDefaultsAndValidate(true); !errors.Is(errors.Join(errs...), ErrRootCredentialRequired) {
		t.Errorf("errs = %v, want ErrRootCredentialRequired", errs)
	}

	with := AuthConfig{Root: RootCredential{AccessKeyID: "AK", SecretAccessKey: "SK"}}
	if errs := with.setDefaultsAndValidate(true); len(errs) != 0 {
		t.Errorf("errs = %v, want none when a root credential is declared", errs)
	}
}
