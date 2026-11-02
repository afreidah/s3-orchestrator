// -------------------------------------------------------------------------------
// Notification Configuration
//
// Author: Alex Freidah
//
// Configuration for webhook event notifications. Endpoints receive CloudEvents
// JSON payloads for configured event types. Supports wildcard event filtering,
// optional prefix matching for data events, HMAC-SHA256 signing, and per-
// endpoint timeout and retry settings.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"time"
)

// NotificationConfig holds the list of webhook endpoints for event delivery.
// An empty Endpoints slice disables notifications entirely.
type NotificationConfig struct {
	Endpoints []NotificationEndpoint `yaml:"endpoints"`
}

// NotificationEndpoint configures a single webhook destination.
type NotificationEndpoint struct {
	URL        string        `yaml:"url"`         // Webhook URL (required)
	Events     []string      `yaml:"events"`      // Event type patterns with wildcard support (required)
	Prefix     string        `yaml:"prefix"`      // Only emit data events for keys matching this prefix (optional)
	Secret     string        `yaml:"secret"`      //nolint:gosec // G117: config struct, not a hardcoded credential
	Timeout    time.Duration `yaml:"timeout"`     // HTTP request timeout (default: 5s)
	MaxRetries int           `yaml:"max_retries"` // Delivery attempts before dropping (default: 3)
}

// setDefaultsAndValidate sets defaults and validate.
func (c *NotificationConfig) setDefaultsAndValidate() []error {
	var errs []error
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		path := fmt.Sprintf("notifications.endpoints[%d]", i)
		ep.Timeout = cmp.Or(ep.Timeout, 5*time.Second)
		ep.MaxRetries = cmp.Or(ep.MaxRetries, 3)
		errs = append(errs, failed(
			required(path+".url", ep.URL != ""),
			required(path+".events", len(ep.Events) > 0),
			positive(path+".timeout", ep.Timeout),
			positive(path+".max_retries", ep.MaxRetries),
		)...)
	}
	return errs
}
