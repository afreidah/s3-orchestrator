// -------------------------------------------------------------------------------
// Helper Tests - Path Parsing, XML Escaping, and Metadata Extraction
//
// Author: Alex Freidah
//
// Unit tests for URL path parsing (bucket/key extraction), XML special
// character escaping, and x-amz-meta-* header extraction/validation.
// -------------------------------------------------------------------------------

package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantBucket string
		wantKey    string
		wantOk     bool
	}{
		{
			name:       "bucket and key",
			path:       "/mybucket/mykey",
			wantBucket: "mybucket",
			wantKey:    "mykey",
			wantOk:     true,
		},
		{
			name:       "bucket and nested key",
			path:       "/mybucket/path/to/object.jpg",
			wantBucket: "mybucket",
			wantKey:    "path/to/object.jpg",
			wantOk:     true,
		},
		{
			name:       "bucket only with trailing slash",
			path:       "/mybucket/",
			wantBucket: "mybucket",
			wantKey:    "",
			wantOk:     true,
		},
		{
			name:       "bucket only no trailing slash",
			path:       "/mybucket",
			wantBucket: "mybucket",
			wantKey:    "",
			wantOk:     true,
		},
		{
			name:   "empty path",
			path:   "/",
			wantOk: false,
		},
		{
			name:   "bare empty",
			path:   "",
			wantOk: false,
		},
		{
			name:       "key with spaces",
			path:       "/bucket/my file.txt",
			wantBucket: "bucket",
			wantKey:    "my file.txt",
			wantOk:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bucket, key, ok := parsePath(tt.path)
			if ok != tt.wantOk {
				t.Errorf("parsePath(%q) ok = %v, want %v", tt.path, ok, tt.wantOk)
			}
			if ok {
				if bucket != tt.wantBucket {
					t.Errorf("parsePath(%q) bucket = %q, want %q", tt.path, bucket, tt.wantBucket)
				}
				if key != tt.wantKey {
					t.Errorf("parsePath(%q) key = %q, want %q", tt.path, key, tt.wantKey)
				}
			}
		})
	}
}

func TestIsValidRequestID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"empty", "", false},
		{"valid hex lowercase", "abcdef0123456789", true},
		{"valid hex uppercase", "ABCDEF0123456789", true},
		{"valid hex mixed", "aB12cD34eF56", true},
		{"32-char hex (typical)", "abcdef0123456789abcdef0123456789", true},
		{"64-char hex (max)", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
		{"65 chars (too long)", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567890", false},
		{"contains newline", "abc\ndef", false},
		{"contains carriage return", "abc\rdef", false},
		{"contains space", "abc def", false},
		{"contains dash", "abc-def", false},
		{"contains slash", "abc/def", false},
		{"non-hex letter g", "abcdefg", false},
		{"log injection attempt", "abc\n{\"audit\":true,\"event\":\"fake\"}", false},
		{"header injection", "abc\r\nX-Evil: true", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidRequestID(tt.id)
			if got != tt.want {
				t.Errorf("isValidRequestID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestXmlEscape(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"a&b", "a&amp;b"},
		{"<tag>", "&lt;tag&gt;"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"it's", "it&apos;s"},
		{"a&b<c>d\"e'f", "a&amp;b&lt;c&gt;d&quot;e&apos;f"},
	}

	for _, tt := range tests {
		got := xmlEscape(tt.input)
		if got != tt.want {
			t.Errorf("xmlEscape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// -------------------------------------------------------------------------
// extractUserMetadata
// -------------------------------------------------------------------------

func TestExtractUserMetadata_Basic(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Meta-Project", "acme")
	h.Set("X-Amz-Meta-Env", "prod")
	h.Set("Content-Type", "text/plain")

	meta := extractUserMetadata(h)
	if len(meta) != 2 {
		t.Fatalf("got %d keys, want 2", len(meta))
	}
	if meta["project"] != "acme" {
		t.Errorf("project = %q, want acme", meta["project"])
	}
	if meta["env"] != "prod" {
		t.Errorf("env = %q, want prod", meta["env"])
	}
}

func TestExtractUserMetadata_Empty(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/plain")

	meta := extractUserMetadata(h)
	if meta != nil {
		t.Errorf("expected nil, got %v", meta)
	}
}

func TestExtractUserMetadata_BarePrefix(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Meta-", "value")

	meta := extractUserMetadata(h)
	if meta != nil {
		t.Errorf("expected nil for bare x-amz-meta- prefix, got %v", meta)
	}
}

func TestExtractUserMetadata_CaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set("x-amz-meta-UPPER", "val")

	meta := extractUserMetadata(h)
	if meta["upper"] != "val" {
		t.Errorf("upper = %q, want val", meta["upper"])
	}
}

// -------------------------------------------------------------------------
// validateUserMetadata
// -------------------------------------------------------------------------

func TestValidateUserMetadata_WithinLimit(t *testing.T) {
	meta := map[string]string{"key": "value"}
	if err := validateUserMetadata(meta); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateUserMetadata_ExceedsLimit(t *testing.T) {
	meta := map[string]string{"k": strings.Repeat("x", maxUserMetadataBytes+1)}
	err := validateUserMetadata(meta)
	if err == nil {
		t.Fatal("expected error for oversized metadata")
	}
}

func TestValidateUserMetadata_ExactLimit(t *testing.T) {
	meta := map[string]string{"k": strings.Repeat("x", maxUserMetadataBytes-1)}
	if err := validateUserMetadata(meta); err != nil {
		t.Errorf("unexpected error at exact limit: %v", err)
	}
}
