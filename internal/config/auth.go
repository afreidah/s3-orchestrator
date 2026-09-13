// -------------------------------------------------------------------------------
// Authentication Configuration
//
// Author: Alex Freidah
//
// Defines AuthConfig - the root credential a deployment declares to administer
// itself before any credential exists in the store. It is an ordinary keypair
// resolving to an ordinary user, and what makes it root is the grants that user
// holds rather than a branch in the request path.
//
// Declaring one is what lets the shared admin token be retired: the token and
// the dashboard login both resolve onto this same identity, so a deployment has
// one principal to reason about instead of three mechanisms.
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

// Declared reports whether a root credential was configured at all. A
// deployment may run without one and administer itself through the store.
func (r RootCredential) Declared() bool {
	return r.AccessKeyID != "" || r.SecretAccessKey != ""
}

// AuthConfig holds the credentials a deployment declares for itself, as opposed
// to the ones it issues to clients.
//
// LegacySharedToken is not declared here: it is ui.admin_token, copied in
// during validation so that everything asking who administers a deployment has
// one place to read rather than two spellings of the same idea.
type AuthConfig struct {
	Root              RootCredential `yaml:"root"`
	LegacySharedToken string         `yaml:"-"`
}

// HasRoot reports whether the deployment declares an administering identity by
// either mechanism.
func (a AuthConfig) HasRoot() bool {
	return a.Root.Declared() || a.LegacySharedToken != ""
}

// setDefaultsAndValidate refuses a half-declared root credential.
func (a *AuthConfig) setDefaultsAndValidate() []error {
	if !a.Root.Declared() {
		return nil
	}
	if a.Root.AccessKeyID == "" || a.Root.SecretAccessKey == "" {
		return []error{ErrRootCredentialIncomplete}
	}
	return nil
}
