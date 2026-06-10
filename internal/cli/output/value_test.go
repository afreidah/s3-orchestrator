// -------------------------------------------------------------------------------
// CLI Output - Human-Readable JSON Value Rendering Tests
//
// Author: Alex Freidah
//
// Covers scalar formatting (numbers without exponents, booleans, null, strings),
// nested object/array indentation, deterministic key ordering, and the
// non-JSON passthrough.
// -------------------------------------------------------------------------------

package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderValue_FlatObject(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`{"imported":3,"removed":0,"status":"ok"}`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	// Keys render sorted, scalars without quoting or float noise.
	want := "imported: 3\nremoved: 0\nstatus: ok\n"
	if got := buf.String(); got != want {
		t.Errorf("RenderValue:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderValue_LargeIntegerNoExponent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`{"bytes":1234567890}`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	if got := buf.String(); got != "bytes: 1234567890\n" {
		t.Errorf("large integer rendered with exponent/noise: %q", got)
	}
}

func TestRenderValue_Scalars(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`{"a":true,"b":null,"c":1.5,"d":"text"}`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	want := "a: true\nb: null\nc: 1.5\nd: text\n"
	if got := buf.String(); got != want {
		t.Errorf("scalar rendering:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderValue_NestedObject(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`{"outer":{"inner":1}}`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	want := "outer:\n  inner: 1\n"
	if got := buf.String(); got != want {
		t.Errorf("nested object:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderValue_ArrayOfScalars(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`{"backends":["gcp","b2"]}`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	want := "backends:\n  - gcp\n  - b2\n"
	if got := buf.String(); got != want {
		t.Errorf("array of scalars:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderValue_ArrayOfObjects(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`[{"name":"gcp","objects":5}]`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	want := "-\n  name: gcp\n  objects: 5\n"
	if got := buf.String(); got != want {
		t.Errorf("array of objects:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderValue_TopLevelScalar(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`"just a string"`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	if got := buf.String(); got != "just a string\n" {
		t.Errorf("top-level scalar = %q", got)
	}
}

func TestRenderValue_InvalidPassesThrough(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderValue(&buf, []byte(`not json`)); err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	if got := buf.String(); got != "not json\n" {
		t.Errorf("passthrough = %q", got)
	}
}

func TestRenderValue_WriteError(t *testing.T) {
	t.Parallel()
	if err := RenderValue(errWriter{}, []byte(`{"a":1}`)); err == nil {
		t.Error("expected write error on valid JSON, got nil")
	}
	if err := RenderValue(errWriter{}, []byte(`not json`)); err == nil {
		t.Error("expected write error on passthrough, got nil")
	}
}

func TestScalar_NonJSONTypeFallback(t *testing.T) {
	t.Parallel()
	// JSON decoding never yields these, but scalar must still format any value
	// passed to it rather than panic or return empty.
	if got := scalar(42); got != "42" {
		t.Errorf("scalar(int) = %q, want %q", got, "42")
	}
}

func TestRenderValue_DeterministicKeyOrder(t *testing.T) {
	t.Parallel()
	// Same input rendered twice must produce identical output despite Go's
	// randomized map iteration.
	in := []byte(`{"z":1,"a":2,"m":3}`)
	var a, b bytes.Buffer
	_ = RenderValue(&a, in)
	_ = RenderValue(&b, in)
	if a.String() != b.String() {
		t.Errorf("non-deterministic output:\n%q\nvs\n%q", a.String(), b.String())
	}
	if !strings.HasPrefix(a.String(), "a: 2\n") {
		t.Errorf("keys not sorted: %q", a.String())
	}
}
