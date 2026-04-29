// -------------------------------------------------------------------------------
// Internal Key Tests
//
// Author: Alex Freidah
//
// Round-trip and edge-case coverage for the bucket / user-key helpers.
// -------------------------------------------------------------------------------

package internalkey

import "testing"

func TestMakeAndSplit_RoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bucket, userKey string
	}{
		{"alpha", "photos/2026/img.jpg"},
		{"alpha", "single"},
		{"alpha", ""},
		{"alpha", "/leading-slash"},
		{"alpha", "trailing/"},
	}
	for _, tt := range tests {
		got := Make(tt.bucket, tt.userKey)
		gotBucket, gotKey := Split(got)
		if gotBucket != tt.bucket || gotKey != tt.userKey {
			t.Errorf("Make(%q, %q) -> %q -> Split = (%q, %q)",
				tt.bucket, tt.userKey, got, gotBucket, gotKey)
		}
	}
}

func TestSplit_NoSeparator(t *testing.T) {
	t.Parallel()
	bucket, userKey := Split("orphan")
	if bucket != "orphan" || userKey != "" {
		t.Errorf("Split(\"orphan\") = (%q, %q), want (\"orphan\", \"\")", bucket, userKey)
	}
}

func TestPrefix(t *testing.T) {
	t.Parallel()
	if got := Prefix("alpha"); got != "alpha/" {
		t.Errorf("Prefix(\"alpha\") = %q, want \"alpha/\"", got)
	}
}
