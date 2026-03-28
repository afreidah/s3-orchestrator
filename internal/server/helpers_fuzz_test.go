// -------------------------------------------------------------------------------
// Helpers Fuzz Tests - URL Path Parsing and Metadata Validation
//
// Author: Alex Freidah
//
// Fuzz tests for HTTP request path parsing and user metadata validation.
// Validates structural invariants on fuzzed output.
// -------------------------------------------------------------------------------

package server

import (
	"strings"
	"testing"
)

func FuzzParsePath(f *testing.F) {
	f.Add("/bucket/key")
	f.Add("/bucket")
	f.Add("/")
	f.Add("")
	f.Add("//")
	f.Add("/bucket/a/b/c/d/e/f")
	f.Add("no-leading-slash")
	f.Add("/bucket/")
	f.Add("///")
	f.Add("/bucket/key with spaces")
	f.Add("/bucket/%2Fencoded")
	f.Add("/\x00null/key")

	f.Fuzz(func(t *testing.T, path string) {
		bucket, key, ok := parsePath(path)
		if ok && bucket == "" {
			t.Errorf("parsePath(%q) returned ok=true with empty bucket", path)
		}
		// Bucket is the first path segment; it must not contain a slash.
		if ok && strings.Contains(bucket, "/") {
			t.Errorf("parsePath(%q) returned bucket %q containing slash", path, bucket)
		}
		// When key is non-empty, the original path (trimmed) should start
		// with bucket + "/".
		if ok && key != "" {
			trimmed := strings.TrimPrefix(path, "/")
			if !strings.HasPrefix(trimmed, bucket+"/") {
				t.Errorf("parsePath(%q): bucket=%q key=%q does not reconstruct", path, bucket, key)
			}
		}
	})
}

func FuzzValidMetadataToken(f *testing.F) {
	f.Add("normal-key")
	f.Add("value\r\nInjected: header")
	f.Add("\x00")
	f.Add(strings.Repeat("a", 3000))
	f.Add("")
	f.Add("hello world")
	f.Add("\t\n\r")

	f.Fuzz(func(t *testing.T, input string) {
		result := validMetadataToken(input)
		if result {
			// validMetadataToken accepts only printable ASCII (0x20-0x7E).
			for _, c := range input {
				if c < 0x20 || c > 0x7E {
					t.Errorf("accepted char %U in %q", c, input)
				}
			}
		}
	})
}
