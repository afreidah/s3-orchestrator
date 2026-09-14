// -------------------------------------------------------------------------------
// UI Configuration
//
// Author: Alex Freidah
//
// Defines UIConfig - the optional admin dashboard block. Disabled by default.
// Carries the session HMAC secret used to derive the cookie signing key and the
// static-asset path.
//
// The dashboard has no login of its own: it authenticates the same credentials
// the S3 and admin APIs do, resolving one to the user behind it. What a session
// carries is that user, so the session secret is configured separately from any
// credential and rotating one does not touch the other.
// -------------------------------------------------------------------------------

package config

import "cmp"

// UIConfig holds settings for the built-in web dashboard. Disabled by default.
type UIConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`                 // URL prefix for the dashboard (default: "/ui")
	SessionSecret      string `yaml:"session_secret"`       //nolint:gosec // G117: config struct, not a hardcoded credential  -  HMAC key for session cookie derivation
	ForceSecureCookies bool   `yaml:"force_secure_cookies"` // Always set Secure flag on session cookies (use behind TLS-terminating proxy)
}

// setDefaultsAndValidate sets defaults and validate.
func (u *UIConfig) setDefaultsAndValidate() []error {
	var errs []error

	u.Path = cmp.Or(u.Path, "/ui")
	if u.Enabled && u.SessionSecret == "" {
		errs = append(errs, ErrSessionSecretReqd)
	}

	return errs
}
