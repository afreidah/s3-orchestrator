// -------------------------------------------------------------------------------
// Helper Tests - Path Parsing, XML Escaping, and Metadata Extraction
//
// Author: Alex Freidah
//
// Unit tests for URL path parsing (bucket/key extraction), XML special
// character escaping, and x-amz-meta-* header extraction/validation.
// -------------------------------------------------------------------------------

package s3api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestParsePath verifies the parse path contract.
// Asserts that parsePath() ok = , want.
func TestParsePath(t *testing.T) {
	t.Parallel()
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

// TestIsValidRequestID verifies the is valid request id contract.
// Asserts that isValidRequestID() = , want.
func TestIsValidRequestID(t *testing.T) {
	t.Parallel()
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

// TestXmlEscape verifies the xml escape contract.
// Asserts that xmlEscape() = , want.
func TestXmlEscape(t *testing.T) {
	t.Parallel()
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

// TestExtractUserMetadata_Basic verifies the extract user metadata basic contract.
// Asserts that got keys, want 2.
func TestExtractUserMetadata_Basic(t *testing.T) {
	t.Parallel()
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

// TestExtractUserMetadata_Empty verifies the extract user metadata empty contract.
// Asserts that expected nil, got.
func TestExtractUserMetadata_Empty(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Content-Type", "text/plain")

	meta := extractUserMetadata(h)
	if meta != nil {
		t.Errorf("expected nil, got %v", meta)
	}
}

// TestExtractUserMetadata_BarePrefix verifies the extract user metadata bare prefix contract.
// Asserts that expected nil for bare x-amz-meta- prefix, got.
func TestExtractUserMetadata_BarePrefix(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-Amz-Meta-", "value")

	meta := extractUserMetadata(h)
	if meta != nil {
		t.Errorf("expected nil for bare x-amz-meta- prefix, got %v", meta)
	}
}

// TestExtractUserMetadata_CaseInsensitive verifies the extract user metadata case insensitive contract.
// Asserts that upper = , want val.
func TestExtractUserMetadata_CaseInsensitive(t *testing.T) {
	t.Parallel()
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

// TestValidateUserMetadata_WithinLimit verifies the validate user metadata within limit contract.
// Asserts that unexpected error:.
func TestValidateUserMetadata_WithinLimit(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"key": "value"}
	if err := validateUserMetadata(meta); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidateUserMetadata_ExceedsLimit verifies the validate user metadata exceeds limit path by exercising strings.Repeat.
func TestValidateUserMetadata_ExceedsLimit(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"k": strings.Repeat("x", maxUserMetadataBytes+1)}
	err := validateUserMetadata(meta)
	if err == nil {
		t.Fatal("expected error for oversized metadata")
	}
}

// TestValidateUserMetadata_ExactLimit verifies the validate user metadata exact limit contract.
// Asserts that unexpected error at exact limit:.
func TestValidateUserMetadata_ExactLimit(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"k": strings.Repeat("x", maxUserMetadataBytes-1)}
	if err := validateUserMetadata(meta); err != nil {
		t.Errorf("unexpected error at exact limit: %v", err)
	}
}

// TestFormatCapacityHint_EmptyStatsReturnsEmpty verifies that the
// capacity-hint formatter returns "" when no stats are present so the
// caller falls back to its terse default error message.
func TestFormatCapacityHint_EmptyStatsReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := formatCapacityHint(nil); got != "" {
		t.Errorf("formatCapacityHint(nil) = %q, want empty", got)
	}
	if got := formatCapacityHint(map[string]core.QuotaStat{}); got != "" {
		t.Errorf("formatCapacityHint(empty) = %q, want empty", got)
	}
}

// TestFormatCapacityHint_RendersSortedSummary verifies the formatter
// produces a deterministic comma-separated "name=used/limit" summary.
// Output is sorted by backend name so the message is stable across runs.
func TestFormatCapacityHint_RendersSortedSummary(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"oci": {BackendName: "oci", BytesUsed: 1_181_116_006, BytesLimit: 10_737_418_240},
		"r2":  {BackendName: "r2", BytesUsed: 1_395_864_371, BytesLimit: 10_737_418_240},
		"e2":  {BackendName: "e2", BytesUsed: 0, BytesLimit: 10_737_418_240},
	}
	got := formatCapacityHint(stats)
	want := "e2=0 B/10.0 GiB, oci=1.1 GiB/10.0 GiB, r2=1.3 GiB/10.0 GiB"
	if got != want {
		t.Errorf("formatCapacityHint = %q\nwant %q", got, want)
	}
}

// TestValidateUserMetadata_RejectsCRLFInKey verifies the validate user metadata rejects crlfin key behaviour described by the test name.
// Also asserts the error message names the offending key, the bad byte
// in hex, and the position so operators can fix the request without
// inspecting raw bytes.
func TestValidateUserMetadata_RejectsCRLFInKey(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"bad\r\nkey": "value"}
	err := validateUserMetadata(meta)
	if err == nil {
		t.Fatal("expected error for key containing CRLF")
	}
	msg := err.Error()
	for _, want := range []string{`"bad`, "0x0d", "position 3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing substring %q", msg, want)
		}
	}
}

// TestValidateUserMetadata_RejectsCRLFInValue verifies the validate user metadata rejects crlfin value behaviour described by the test name.
func TestValidateUserMetadata_RejectsCRLFInValue(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"key": "bad\nvalue"}
	if err := validateUserMetadata(meta); err == nil {
		t.Fatal("expected error for value containing newline")
	}
}

// TestValidateUserMetadata_RejectsNullByte verifies the validate user metadata rejects null byte behaviour described by the test name.
func TestValidateUserMetadata_RejectsNullByte(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"key": "val\x00ue"}
	if err := validateUserMetadata(meta); err == nil {
		t.Fatal("expected error for value containing null byte")
	}
}

// TestValidMetadataToken verifies the valid metadata token contract.
// Asserts that validMetadataToken() = , want.
func TestValidMetadataToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"valid-key", true},
		{"hello world", true},
		{"has\nnewline", false},
		{"has\rreturn", false},
		{"has\x00null", false},
		{"has\ttab", false},
		{"\u00fcber", false}, // non-ASCII test input (U+00FC, u-with-diaeresis)
		{"", true},
	}
	for _, tc := range tests {
		if got := validMetadataToken(tc.input); got != tc.want {
			t.Errorf("validMetadataToken(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// writeS3Error
// -------------------------------------------------------------------------

// TestWriteS3Error_SetsContentLength verifies the write s3 error sets content length contract.
// Asserts that status = , want.
func TestWriteS3Error_SetsContentLength(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")

	resp := w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	cl := resp.Header.Get("Content-Length")
	if cl == "" {
		t.Fatal("expected Content-Length header")
	}
	length, err := strconv.Atoi(cl)
	if err != nil {
		t.Fatalf("invalid Content-Length: %v", err)
	}
	if length != w.Body.Len() {
		t.Errorf("Content-Length %d != body length %d", length, w.Body.Len())
	}
}

// TestWriteS3Error_EscapesXML verifies the write s3 error escapes xml path by exercising httptest.NewRecorder, strings.Contains.
func TestWriteS3Error_EscapesXML(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeS3Error(w, http.StatusBadRequest, "Test", "<script>alert('xss')</script>")

	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("XML special characters not escaped in error body")
	}
}

// TestFormatCapacityHint_ClampsNegativeCounters verifies a drifted counter
// renders as "0 B" rather than a negative size. Available space cannot be
// negative in the error message an operator reads, so the clamp lives here
// rather than in the shared byte formatter, which reports negatives faithfully.
func TestFormatCapacityHint_ClampsNegativeCounters(t *testing.T) {
	t.Parallel()
	stats := map[string]core.QuotaStat{
		"drifted": {BackendName: "drifted", BytesUsed: -1, BytesLimit: -1},
	}
	if got, want := formatCapacityHint(stats), "drifted=0 B/0 B"; got != want {
		t.Errorf("formatCapacityHint = %q, want %q", got, want)
	}
}

// decodeTarget is a minimal document shape for the decodeXMLBody tests.
type decodeTarget struct {
	XMLName xml.Name `xml:"Delete"`
	Keys    []string `xml:"Object>Key"`
}

// decodeBody runs decodeXMLBody against body with the given limit.
func decodeBody(t *testing.T, body string, limit int64) (int, *decodeTarget, error) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mybucket?delete", strings.NewReader(body))
	w := httptest.NewRecorder()
	var got decodeTarget
	status, err := decodeXMLBody(w, r, limit, &got)
	return status, &got, err
}

// TestDecodeXMLBody_AcceptsOneDocument verifies an ordinary request decodes.
func TestDecodeXMLBody_AcceptsOneDocument(t *testing.T) {
	t.Parallel()
	status, got, err := decodeBody(t, `<Delete><Object><Key>a.txt</Key></Object></Delete>`, 1024)
	if err != nil {
		t.Fatalf("decodeXMLBody: %v (status %d)", err, status)
	}
	if len(got.Keys) != 1 || got.Keys[0] != "a.txt" {
		t.Errorf("keys = %v, want [a.txt]", got.Keys)
	}
}

// TestDecodeXMLBody_RejectsTrailingContent verifies junk after a complete
// document is refused. Decode stops at the end of the first document, so
// without the second decode this was accepted and the remainder discarded.
func TestDecodeXMLBody_RejectsTrailingContent(t *testing.T) {
	t.Parallel()
	status, _, err := decodeBody(t, `<Delete><Object><Key>a.txt</Key></Object></Delete>garbage`, 1024)
	if err == nil {
		t.Fatal("trailing content must be rejected")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestDecodeXMLBody_RejectsSecondDocument is the case that matters most for a
// batch delete: two documents used to parse as the first one alone, so the
// orchestrator acted on a different key set than the full body described.
func TestDecodeXMLBody_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	body := `<Delete><Object><Key>a.txt</Key></Object></Delete>` +
		`<Delete><Object><Key>b.txt</Key></Object></Delete>`
	status, _, err := decodeBody(t, body, 1024)
	if err == nil {
		t.Fatal("a second document must be rejected")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestDecodeXMLBody_OversizedIsTooLarge verifies an over-limit body is reported
// as too large rather than as malformed XML. A LimitReader truncated instead,
// so a client sending a legal-but-large request was told its XML was broken.
func TestDecodeXMLBody_OversizedIsTooLarge(t *testing.T) {
	t.Parallel()
	body := `<Delete><Object><Key>` + strings.Repeat("x", 512) + `</Key></Object></Delete>`
	status, _, err := decodeBody(t, body, 64)
	if err == nil {
		t.Fatal("an oversized body must be rejected")
	}
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", status)
	}
}

// TestDecodeXMLBody_OversizedAfterCompleteDocument is the silent-truncation
// case. The document closes inside the limit and padding follows, so a
// LimitReader handed back a complete, valid document and threw the rest away:
// nothing failed, and the orchestrator acted on a prefix of what was sent.
//
// The padding is both trailing content and over the ceiling. It is reported as
// trailing content, which is the more actionable of the two - a body that is
// one oversized document still reports too-large, covered separately.
func TestDecodeXMLBody_OversizedAfterCompleteDocument(t *testing.T) {
	t.Parallel()
	body := `<Delete><Object><Key>a.txt</Key></Object></Delete>` + strings.Repeat("x", 4096)
	status, _, err := decodeBody(t, body, 128)
	if err == nil {
		t.Fatal("padding past the limit must not be silently discarded")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}
