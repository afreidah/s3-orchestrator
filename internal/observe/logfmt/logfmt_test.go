// -------------------------------------------------------------------------------
// Logfmt Tests
//
// Author: Alex Freidah
//
// Pins the contract every helper here exists to enforce: the JSON handler
// must emit a printable error string (not "{}"), the canonical attribute
// keys never drift, and a nil/empty value path returns an empty Attr that
// slog drops rather than a noisy zero-value entry.
// -------------------------------------------------------------------------------

package logfmt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// errOpaque is the kind of error that pre-logfmt code path serialised as
// {} via the JSON handler. The struct has no exported fields and no
// MarshalJSON so encoding/json produces an empty object  -  exactly the
// downstream "[object Object]" rendering that motivated the helper.
type errOpaque struct{ inner string }

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Error returns the wrapped message.
func (e *errOpaque) Error() string { return e.inner }

// TestErr_RendersStringNotEmptyObject checks the canonical bug the helper
// was introduced to defeat: a structured error that would otherwise log as
// {} now lands as the string returned by Error().
func TestErr_RendersStringNotEmptyObject(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.LogAttrs(context.Background(), slog.LevelInfo, "probe",
		logfmt.Err(&errOpaque{inner: "vault token expired"}),
	)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decode log entry: %v", err)
	}
	got, ok := entry["error"].(string)
	if !ok {
		t.Fatalf("error attr type = %T, want string", entry["error"])
	}
	if got != "vault token expired" {
		t.Errorf("error = %q, want %q", got, "vault token expired")
	}
}

// TestErr_NilReturnsEmptyAttr verifies the nil-error guard so callers can
// pass logfmt.Err(err) unconditionally without polluting logs with a
// blank "error" field on the success path.
func TestErr_NilReturnsEmptyAttr(t *testing.T) {
	t.Parallel()
	if got := logfmt.Err(nil); !got.Equal(slog.Attr{}) {
		t.Errorf("Err(nil) = %+v, want empty Attr", got)
	}
}

// TestErr_PreservesStandardErrorMessages spot-checks that an errors.New
// value (the common case) round-trips through the helper unchanged.
func TestErr_PreservesStandardErrorMessages(t *testing.T) {
	t.Parallel()
	want := "boom"
	got := logfmt.Err(errors.New(want))
	if got.Key != "error" {
		t.Errorf("Key = %q, want %q", got.Key, "error")
	}
	if got.Value.String() != want {
		t.Errorf("Value = %q, want %q", got.Value.String(), want)
	}
}

// TestOutcomeAndComponent_KeysAreCanonical pins the canonical attribute
// keys so a typo in either constructor would fail loudly  -  these names
// drive Grafana queries and CI lint rules.
func TestOutcomeAndComponent_KeysAreCanonical(t *testing.T) {
	t.Parallel()
	if got := logfmt.Outcome(logfmt.OutcomeOK); got.Key != "outcome" {
		t.Errorf("Outcome key = %q, want outcome", got.Key)
	}
	if got := logfmt.Component("rebalancer"); got.Key != "component" {
		t.Errorf("Component key = %q, want component", got.Key)
	}
}

// TestTransformAttr covers each value kind: an error, including one nested
// in a group, becomes its Error() string; anything else, including an
// interface-nil error, passes through unchanged.
func TestTransformAttr(t *testing.T) {
	t.Parallel()
	var nilErr error
	cases := []struct {
		name string
		in   slog.Attr
		want slog.Attr
	}{
		{"error", slog.Any("error", &errOpaque{inner: "boom"}), slog.String("error", "boom")},
		{"non-error", slog.Int("status", 503), slog.Int("status", 503)},
		{"interface-nil error", slog.Any("error", nilErr), slog.Any("error", nilErr)},
		{
			"group",
			slog.Group("ctx", slog.Any("error", &errOpaque{inner: "nested"}), slog.String("k", "v")),
			slog.Group("ctx", slog.String("error", "nested"), slog.String("k", "v")),
		},
	}
	for _, tc := range cases {
		if got := logfmt.TransformAttr(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: TransformAttr = %v, want %v", tc.name, got, tc.want)
		}
	}
}
