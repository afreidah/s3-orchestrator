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
			errs := a.setDefaultsAndValidate()
			if tc.wantErr && !errors.Is(errors.Join(errs...), ErrRootCredentialIncomplete) {
				t.Errorf("errs = %v, want ErrRootCredentialIncomplete", errs)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("errs = %v, want none", errs)
			}
		})
	}
}

// TestAuthConfig_HasRoot verifies both ways of declaring an administering
// identity are recognised, since the merge builds the root user from either.
func TestAuthConfig_HasRoot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		auth AuthConfig
		want bool
	}{
		{"nothing declared", AuthConfig{}, false},
		{"a keypair", AuthConfig{Root: RootCredential{AccessKeyID: "AK", SecretAccessKey: "SK"}}, true},
		{"the legacy token", AuthConfig{LegacySharedToken: "tok"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.auth.HasRoot(); got != tc.want {
				t.Errorf("HasRoot() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConfig_LegacySharedTokenFallsBackToAdminKey verifies the root identity is
// built from admin_key when admin_token is unset, which is the fallback the
// admin surface has always applied. Missing it would leave such a deployment
// with a token that resolves to nobody.
func TestConfig_LegacySharedTokenFallsBackToAdminKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ui   UIConfig
		want string
	}{
		{"admin_token wins", UIConfig{AdminToken: "tok", AdminKey: "key"}, "tok"},
		{"admin_key is the fallback", UIConfig{AdminKey: "key"}, "key"},
		{"neither", UIConfig{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Config{UI: tc.ui}
			_ = c.SetDefaultsAndValidate()
			if got := c.Auth.LegacySharedToken; got != tc.want {
				t.Errorf("LegacySharedToken = %q, want %q", got, tc.want)
			}
		})
	}
}
