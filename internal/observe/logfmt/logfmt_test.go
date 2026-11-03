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
	"net/http"
	"net/http/httptest"
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

// TestComponent_KeyIsCanonical pins the canonical attribute key so a typo in
// the constructor would fail loudly; the name drives Grafana queries.
func TestComponent_KeyIsCanonical(t *testing.T) {
	t.Parallel()
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

// TestRequest verifies a request is named by method, path and client address
// in that order, ahead of the caller's own attributes, and that RequestFrom
// names the resolved client in place of the peer address.
func TestRequest(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/photos/cat.jpg?x=1", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	extra := slog.Int("status", 200)

	for _, tc := range []struct {
		name   string
		got    []slog.Attr
		client string
	}{
		{"Request", logfmt.Request(r, extra), "10.0.0.1:5000"},
		{"RequestFrom", logfmt.RequestFrom(r, "203.0.113.50", extra), "203.0.113.50"},
	} {
		want := []slog.Attr{
			slog.String("method", http.MethodPut),
			slog.String("path", "/photos/cat.jpg"),
			slog.String("client_addr", tc.client),
			extra,
		}
		if len(tc.got) != len(want) {
			t.Fatalf("%s = %v, want %v", tc.name, tc.got, want)
		}
		for i := range want {
			if !tc.got[i].Equal(want[i]) {
				t.Errorf("%s[%d] = %v, want %v", tc.name, i, tc.got[i], want[i])
			}
		}
	}
}
