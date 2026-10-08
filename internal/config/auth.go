// -------------------------------------------------------------------------------
// Authentication Configuration
//
// Author: Alex Freidah
//
// Defines AuthConfig - the root credential a deployment administers itself with.
// It is an ordinary keypair resolving to an ordinary user, and what makes it
// root is the grants that user holds rather than a branch in the request path.
//
// It is also the only way in. The admin API, the dashboard and the S3 API all
// authenticate the same credential type, so a deployment with no root credential
// and no provisioned one has nothing able to create the first user.
// -------------------------------------------------------------------------------

package config

// RootCredential is the keypair that administers a deployment.
//
// Both halves are required together: a key with no secret cannot sign and a
// secret with no key names nothing, so declaring one alone is a mistake rather
// than a partial configuration.
type RootCredential struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
}

// Declared reports whether a root credential was configured at all.
func (r RootCredential) Declared() bool {
	return r.AccessKeyID != "" || r.SecretAccessKey != ""
}

// Complete reports whether both halves are present, which is what it takes to
// sign.
func (r RootCredential) Complete() bool {
	return r.AccessKeyID != "" && r.SecretAccessKey != ""
}

// AuthConfig holds the credentials a deployment declares for itself, as opposed
// to the ones it issues to clients.
type AuthConfig struct {
	Root RootCredential `yaml:"root"`
}

// HasRoot reports whether the deployment declares an administering identity.
func (a AuthConfig) HasRoot() bool {
	return a.Root.Complete()
}

// setDefaultsAndValidate refuses a half-declared root credential, and a missing
// one when needsRoot is set (the dashboard is enabled and has no other login).
// Without the dashboard, a deployment may run with no root credential and be
// administered by credentials already in its store.
func (a *AuthConfig) setDefaultsAndValidate(needsRoot bool) []error {
	if a.Root.Declared() && !a.Root.Complete() {
		return []error{ErrRootCredentialIncomplete}
	}
	if needsRoot && !a.HasRoot() {
		return []error{ErrRootCredentialRequired}
	}
	return nil
}
