// -------------------------------------------------------------------------------
// Logfmt Handler - Production Contract Test
//
// Author: Alex Freidah
//
// Pins the production logging contract end-to-end: a logger scoped with
// logfmt.Component, wrapped by ErrAttrHandler over the stdlib JSON
// handler, must carry the component attr on every record and render
// error-typed attrs as strings regardless of the call-site shape
// ("error", err / slog.Any("error", err) / logfmt.Err(err)). The
// docs/contributing/logging.md contract treats those three call-site
// forms as canonical; this test fails the suite if the runtime
// composition stops honouring them.
// -------------------------------------------------------------------------------

package logfmt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
)

// productionLogger builds the slog handler stack the runtime composes
// for the daemon, minus the trace-handler integration (which would
// require pulling in opentelemetry just to test stringification).
// The shape that matters here is ErrAttrHandler wrapping the JSON
// handler, plus a logfmt.Component-scoped logger on top.
func productionLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(NewErrAttrHandler(base)).With(Component("test_component"))
}

// decodeOne parses the most recent JSON line out of buf so the test can
// assert on individual fields without depending on string ordering.
func decodeOne(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := buf.Bytes()
	if i := bytes.LastIndexByte(bytes.TrimRight(line, "\n"), '\n'); i >= 0 {
		line = line[i+1:]
	}
	var out map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(line), &out); err != nil {
		t.Fatalf("decode JSON log line %q: %v", string(line), err)
	}
	return out
}

// TestProductionPipeline_RendersErrorAndComponent pins the three
// acceptable error-attribute shapes end-to-end. Each case asserts both
// the component attr (from the scoped logger) and the error attr (from
// the handler stringification).
func TestProductionPipeline_RendersErrorAndComponent(t *testing.T) {
	cases := []struct {
		name string
		emit func(log *slog.Logger, err error)
		want string
	}{
		{
			name: "raw kv pair",
			emit: func(log *slog.Logger, err error) {
				log.ErrorContext(context.Background(), "operation failed", "error", err)
			},
			want: "raw error",
		},
		{
			name: "slog.Any",
			emit: func(log *slog.Logger, err error) {
				log.ErrorContext(context.Background(), "operation failed", slog.Any("error", err))
			},
			want: "any error",
		},
		{
			name: "logfmt.Err",
			emit: func(log *slog.Logger, err error) {
				log.ErrorContext(context.Background(), "operation failed", Err(err))
			},
			want: "logfmt error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := productionLogger(&buf)
			tc.emit(log, errors.New(tc.want))

			record := decodeOne(t, &buf)
			if got := record["component"]; got != "test_component" {
				t.Errorf("component = %v, want test_component", got)
			}
			got, ok := record["error"].(string)
			if !ok {
				t.Fatalf("error attr is not a string (got %T %v)", record["error"], record["error"])
			}
			if got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
			if msg := record["msg"]; msg != "operation failed" {
				t.Errorf("msg = %v, want operation failed", msg)
			}
		})
	}
}

// TestProductionPipeline_NilErrorThroughHelperIsDropped confirms the
// nil-safe contract of logfmt.Err: a nil error becomes an empty attr,
// which slog drops, so no "error" key appears on the record. Call-site
// authors lean on this when err may be nil and the attribute should
// vanish rather than render as an empty string.
func TestProductionPipeline_NilErrorThroughHelperIsDropped(t *testing.T) {
	var buf bytes.Buffer
	log := productionLogger(&buf)
	log.InfoContext(context.Background(), "ok", Err(nil))

	record := decodeOne(t, &buf)
	if _, present := record["error"]; present {
		t.Errorf("error attr should be absent on nil error, got record = %+v", record)
	}
	if got := record["component"]; got != "test_component" {
		t.Errorf("component = %v, want test_component", got)
	}
}

// TestProductionPipeline_RawNilErrorIsSafe confirms passing an
// interface-nil error via raw kv does not panic. slog renders the bare
// nil as JSON null; the handler stringification only runs when the
// concrete value implements the error interface (a typed-nil error
// pointer still satisfies the type assertion and is rendered as "").
func TestProductionPipeline_RawNilErrorIsSafe(t *testing.T) {
	var buf bytes.Buffer
	log := productionLogger(&buf)
	var nilErr error
	log.InfoContext(context.Background(), "ok", "error", nilErr)

	record := decodeOne(t, &buf)
	if v, present := record["error"]; present && v != nil {
		t.Errorf("error = %v, want null/absent for interface-nil", v)
	}
}
