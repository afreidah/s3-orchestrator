// -------------------------------------------------------------------------------
// UI Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

// UIConfig holds settings for the built-in web dashboard. Disabled by default.
type UIConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`                // URL prefix for the dashboard (default: "/ui")
	AdminKey           string `yaml:"admin_key"`           // Access key for dashboard login
	AdminSecret        string `yaml:"admin_secret"`        // Secret key for dashboard login (plaintext or bcrypt hash)
	AdminToken         string `yaml:"admin_token"`         // Separate token for admin API (defaults to admin_key if empty)
	SessionSecret      string `yaml:"session_secret"` //nolint:gosec // G117: config struct, not a hardcoded credential — HMAC key for session cookie derivation (independent of admin_secret)
	ForceSecureCookies bool   `yaml:"force_secure_cookies"` // Always set Secure flag on session cookies (use behind TLS-terminating proxy)
}

func (u *UIConfig) setDefaultsAndValidate() []string {
	var errs []string

	if u.Path == "" {
		u.Path = "/ui"
	}
	if u.Enabled {
		if u.AdminKey == "" || u.AdminSecret == "" {
			errs = append(errs, "ui.admin_key and ui.admin_secret are required when ui is enabled")
		}
		if u.SessionSecret == "" {
			errs = append(errs, "ui.session_secret is required when ui is enabled (used for HMAC session key derivation)")
		}
	}

	return errs
}
