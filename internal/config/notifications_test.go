// -------------------------------------------------------------------------------
// Notifications Config Validation Tests
//
// Author: Alex Freidah
//
// Asserts the validation rules for the optional notifications block: every
// endpoint requires a URL and at least one event filter, and validators
// surface every per-endpoint problem in a single pass instead of returning
// the first error. The aggregated error path matters because operators
// commonly misconfigure several endpoints at once and need them all flagged.
// -------------------------------------------------------------------------------

package config

import (
	"strings"
	"testing"
)

// TestNotificationConfig_ValidMinimal verifies the notification config valid minimal contract.
// Asserts that unexpected errors:.
func TestNotificationConfig_ValidMinimal(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{URL: "https://example.com/hook", Events: []string{"*"}},
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
}

// TestNotificationConfig_EmptyEndpoints verifies the notification config empty endpoints contract.
// Asserts that empty endpoints should be valid (disabled), got:.
func TestNotificationConfig_EmptyEndpoints(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 0 {
		t.Errorf("empty endpoints should be valid (disabled), got: %v", errs)
	}
}

// TestNotificationConfig_MissingURL verifies the notification config missing url contract.
// Asserts that expected url error, got:.
func TestNotificationConfig_MissingURL(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{Events: []string{"*"}},
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "url is required") {
		t.Errorf("expected url error, got: %v", errs)
	}
}

// TestNotificationConfig_MissingEvents verifies the notification config missing events contract.
// Asserts that expected events error, got:.
func TestNotificationConfig_MissingEvents(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{URL: "https://example.com"},
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "event pattern") {
		t.Errorf("expected events error, got: %v", errs)
	}
}

// TestNotificationConfig_MultipleErrors verifies the notification config multiple errors contract.
// Asserts that expected 2 errors, got :.
func TestNotificationConfig_MultipleErrors(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{}, // missing both url and events
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 2 {
		t.Errorf("expected 2 errors, got %d: %v", len(errs), errs)
	}
}

// TestNotificationConfig_DefaultTimeout verifies the notification config default timeout contract.
// Asserts that default timeout = , want 5s.
func TestNotificationConfig_DefaultTimeout(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{URL: "https://example.com", Events: []string{"*"}},
		},
	}
	cfg.setDefaultsAndValidate()
	if cfg.Endpoints[0].Timeout != 5_000_000_000 { // 5s in nanoseconds
		t.Errorf("default timeout = %v, want 5s", cfg.Endpoints[0].Timeout)
	}
}

// TestNotificationConfig_DefaultMaxRetries verifies the notification config default max retries contract.
// Asserts that default max_retries = , want 3.
func TestNotificationConfig_DefaultMaxRetries(t *testing.T) {
	t.Parallel()
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{URL: "https://example.com", Events: []string{"*"}},
		},
	}
	cfg.setDefaultsAndValidate()
	if cfg.Endpoints[0].MaxRetries != 3 {
		t.Errorf("default max_retries = %d, want 3", cfg.Endpoints[0].MaxRetries)
	}
}
