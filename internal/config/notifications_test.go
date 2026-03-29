package config

import (
	"strings"
	"testing"
)

func TestNotificationConfig_ValidMinimal(t *testing.T) {
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

func TestNotificationConfig_EmptyEndpoints(t *testing.T) {
	cfg := NotificationConfig{}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 0 {
		t.Errorf("empty endpoints should be valid (disabled), got: %v", errs)
	}
}

func TestNotificationConfig_MissingURL(t *testing.T) {
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{Events: []string{"*"}},
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 1 || !strings.Contains(errs[0], "url is required") {
		t.Errorf("expected url error, got: %v", errs)
	}
}

func TestNotificationConfig_MissingEvents(t *testing.T) {
	cfg := NotificationConfig{
		Endpoints: []NotificationEndpoint{
			{URL: "https://example.com"},
		},
	}
	errs := cfg.setDefaultsAndValidate()
	if len(errs) != 1 || !strings.Contains(errs[0], "event pattern") {
		t.Errorf("expected events error, got: %v", errs)
	}
}

func TestNotificationConfig_MultipleErrors(t *testing.T) {
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

func TestNotificationConfig_DefaultTimeout(t *testing.T) {
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

func TestNotificationConfig_DefaultMaxRetries(t *testing.T) {
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
