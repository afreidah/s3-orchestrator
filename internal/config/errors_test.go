// -------------------------------------------------------------------------------
// Config Error Helper Tests
//
// Author: Alex Freidah
//
// Pins the contract of the small wrapper helpers used by config validators
// and the loader. Each helper composes a sentinel + context into an error
// chain that stays unwrappable via errors.Is so tests and operators can
// match on the sentinel without depending on the surface message.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// wrappedPath
// -------------------------------------------------------------------------

// TestWrappedPath_PreservesSentinelAndCause verifies the wrappedPath
// helper produces an error chain that still matches both the loader
// sentinel and the underlying cause via errors.Is, and that the path
// is rendered in quotes inside the message string.
func TestWrappedPath_PreservesSentinelAndCause(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("loader sentinel")
	cause := errors.New("underlying cause")
	path := "/etc/s3-orchestrator/config.yaml"

	got := wrappedPath(sentinel, path, cause)
	if got == nil {
		t.Fatal("wrappedPath returned nil")
	}
	if !errors.Is(got, sentinel) {
		t.Errorf("errors.Is(got, sentinel) = false, want true; got: %v", got)
	}
	if !errors.Is(got, cause) {
		t.Errorf("errors.Is(got, cause) = false, want true; got: %v", got)
	}
	if !strings.Contains(got.Error(), `"`+path+`"`) {
		t.Errorf("error message %q missing quoted path %q", got.Error(), path)
	}
}

// TestWrappedPath_LoaderSentinelsMatch verifies wrappedPath used with
// each of the LoadConfig sentinels (ErrReadConfigFile, ErrParseConfig,
// ErrInvalidConfig) produces an errors.Is-matchable chain. This is the
// guarantee LoadConfig callers rely on when distinguishing the three
// failure modes.
func TestWrappedPath_LoaderSentinelsMatch(t *testing.T) {
	t.Parallel()

	cause := errors.New("cause")
	cases := []struct {
		name     string
		sentinel error
	}{
		{"read", ErrReadConfigFile},
		{"parse", ErrParseConfig},
		{"invalid", ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrappedPath(tc.sentinel, "/cfg.yaml", cause)
			if !errors.Is(got, tc.sentinel) {
				t.Errorf("errors.Is(got, %v) = false, want true", tc.sentinel)
			}
		})
	}
}
