// -------------------------------------------------------------------------------
// CLI Output - Format Selection and JSON Rendering Tests
//
// Author: Alex Freidah
//
// Covers the --json flag mapping and PrettyJSON's indent-or-passthrough
// behaviour, including the invalid-JSON fallback that must still reach the
// operator.
// -------------------------------------------------------------------------------

package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestFromJSON(t *testing.T) {
	t.Parallel()
	if got := FromJSON(true); got != FormatJSON {
		t.Errorf("FromJSON(true) = %q, want %q", got, FormatJSON)
	}
	if got := FromJSON(false); got != FormatText {
		t.Errorf("FromJSON(false) = %q, want %q", got, FormatText)
	}
}

func TestFormat_IsJSON(t *testing.T) {
	t.Parallel()
	if !FormatJSON.IsJSON() {
		t.Error("FormatJSON.IsJSON() = false, want true")
	}
	if FormatText.IsJSON() {
		t.Error("FormatText.IsJSON() = true, want false")
	}
}

func TestPrettyJSON_IndentsValidJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := PrettyJSON(&buf, []byte(`{"a":1,"b":[2,3]}`)); err != nil {
		t.Fatalf("PrettyJSON: %v", err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": [\n    2,\n    3\n  ]\n}\n"
	if got := buf.String(); got != want {
		t.Errorf("PrettyJSON output:\n%q\nwant:\n%q", got, want)
	}
}

func TestPrettyJSON_TrailingNewline(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := PrettyJSON(&buf, []byte(`{"status":"ok"}`)); err != nil {
		t.Fatalf("PrettyJSON: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "}\n") {
		t.Errorf("expected trailing newline after closing brace, got %q", buf.String())
	}
}

func TestPrettyJSON_InvalidPassesThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{"plain text error body", "not json at all"},
		{"empty", ""},
		{"truncated", `{"a":`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := PrettyJSON(&buf, []byte(tc.raw)); err != nil {
				t.Fatalf("PrettyJSON: %v", err)
			}
			want := tc.raw + "\n"
			if got := buf.String(); got != want {
				t.Errorf("passthrough = %q, want %q", got, want)
			}
		})
	}
}
