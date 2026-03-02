package server

import "testing"

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

	f.Fuzz(func(t *testing.T, path string) {
		bucket, _, ok := parsePath(path)
		// If ok is true, bucket must be non-empty.
		if ok && bucket == "" {
			t.Errorf("parsePath(%q) returned ok=true with empty bucket", path)
		}
	})
}
