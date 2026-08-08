// -------------------------------------------------------------------------------
// UI Configuration
//
// Author: Alex Freidah
//
// Defines UIConfig - the optional admin dashboard block. Disabled by
// default. Carries the session HMAC secret used to derive the cookie
// signing key (independent of admin_secret), the admin password, and
// the static-asset path. The session secret is configured separately
// from admin_secret so rotating one does not force re-authentication of
// every active admin session.
// -------------------------------------------------------------------------------

package config

import "cmp"

// UIConfig holds settings for the built-in web dashboard. Disabled by default.
type UIConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`                 // URL prefix for the dashboard (default: "/ui")
	AdminKey           string `yaml:"admin_key"`            // Access key for dashboard login
	AdminSecret        string `yaml:"admin_secret"`         // Secret key for dashboard login (plaintext or bcrypt hash)
	AdminToken         string `yaml:"admin_token"`          // Separate token for admin API (defaults to admin_key if empty)
	SessionSecret      string `yaml:"session_secret"`       //nolint:gosec // G117: config struct, not a hardcoded credential  -  HMAC key for session cookie derivation (independent of admin_secret)
	ForceSecureCookies bool   `yaml:"force_secure_cookies"` // Always set Secure flag on session cookies (use behind TLS-terminating proxy)
}

// setDefaultsAndValidate sets defaults and validate.
func (u *UIConfig) setDefaultsAndValidate() []error {
	var errs []error

	u.Path = cmp.Or(u.Path, "/ui")
	if u.Enabled {
		if u.AdminKey == "" || u.AdminSecret == "" {
			errs = append(errs, ErrAdminAuthIncomplete)
		}
		if u.SessionSecret == "" {
			errs = append(errs, ErrSessionSecretReqd)
		}
	}

	return errs
}
