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
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// errOpaque is the kind of error that pre-logfmt code path serialised as
// {} via the JSON handler. The struct has no exported fields and no
// MarshalJSON so encoding/json produces an empty object  -  exactly the
// downstream "[object Object]" footgun that motivated the helper.
type errOpaque struct{ inner string }

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

// TestErrAttrHandler_StringifiesRawError pins the bug-fix contract: a
// raw error passed via slog's key-value form lands as a string in the
// JSON output rather than the empty-object the default handler would
// produce. This is the production fix the codemod previously achieved
// per call site; the handler now does it uniformly.
func TestErrAttrHandler_StringifiesRawError(t *testing.T) {
	t.Parallel()

	mkErr := func(s string) error { return &opaqueErrType{msg: s} }

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(logfmt.NewErrAttrHandler(inner))

	logger.LogAttrs(context.Background(), slog.LevelInfo, "probe",
		slog.Any("error", mkErr("vault token expired")),
	)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := entry["error"].(string); got != "vault token expired" {
		t.Errorf("error = %v, want vault token expired", entry["error"])
	}
}

// opaqueErrType is the kind of error that pre-handler code path would
// have rendered as {} via encoding/json.
type opaqueErrType struct{ msg string }

func (e *opaqueErrType) Error() string { return e.msg }

// TestErrAttrHandler_PreservesNonErrorAttrs verifies that non-error
// values pass through unchanged.
func TestErrAttrHandler_PreservesNonErrorAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(logfmt.NewErrAttrHandler(inner))

	logger.LogAttrs(context.Background(), slog.LevelInfo, "probe",
		slog.String("backend", "e2"),
		slog.Int("status", 503),
	)

	if !strings.Contains(buf.String(), `"backend":"e2"`) {
		t.Errorf("missing backend attr: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":503`) {
		t.Errorf("missing status attr: %s", buf.String())
	}
}

// TestErrAttrHandler_RecursesIntoGroups verifies group attributes are
// walked so an error nested under a group still renders as a string.
func TestErrAttrHandler_RecursesIntoGroups(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(logfmt.NewErrAttrHandler(inner))

	logger.LogAttrs(context.Background(), slog.LevelInfo, "probe",
		slog.Group("ctx",
			slog.Any("error", &opaqueErrType{msg: "nested boom"}),
		),
	)

	if !strings.Contains(buf.String(), `"error":"nested boom"`) {
		t.Errorf("group recursion failed: %s", buf.String())
	}
}

// TestErrAttrHandler_WithAttrsForwards verifies that errors passed via
// logger.With(...) are stringified the same way as record-time attrs.
func TestErrAttrHandler_WithAttrsForwards(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(logfmt.NewErrAttrHandler(inner))
	scoped := logger.With("error", &opaqueErrType{msg: "scoped boom"})
	scoped.LogAttrs(context.Background(), slog.LevelInfo, "probe")

	if !strings.Contains(buf.String(), `"error":"scoped boom"`) {
		t.Errorf("With(...) attr not stringified: %s", buf.String())
	}
}

// TestErrAttrHandler_EnabledForwards confirms the Enabled delegation.
func TestErrAttrHandler_EnabledForwards(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := logfmt.NewErrAttrHandler(inner)
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Debug should be disabled when inner level is Warn")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Error should be enabled when inner level is Warn")
	}
}

// TestErrAttrHandler_WithGroupForwards confirms group-name pass-through.
func TestErrAttrHandler_WithGroupForwards(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	h := logfmt.NewErrAttrHandler(inner)
	scoped := slog.New(h.WithGroup("ctx"))
	scoped.LogAttrs(context.Background(), slog.LevelInfo, "probe", slog.String("k", "v"))
	if !strings.Contains(buf.String(), `"ctx":{"k":"v"}`) {
		t.Errorf("group not applied: %s", buf.String())
	}
}
