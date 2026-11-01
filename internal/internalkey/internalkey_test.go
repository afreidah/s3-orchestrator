// -------------------------------------------------------------------------------
// Internal Key Tests
//
// Author: Alex Freidah
//
// Round-trip and edge-case coverage for the bucket / user-key helpers.
// -------------------------------------------------------------------------------

package internalkey

import "testing"

// TestMakeAndSplit_RoundTrip verifies the make and split round trip contract.
// Asserts that Make(, ) -> -> Split = (, ).
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

// TestSplit_NoSeparator verifies the split no separator contract.
// Asserts that Split(\"orphan\") = (, ), want (\"orphan\", \"\").
func TestSplit_NoSeparator(t *testing.T) {
	t.Parallel()
	bucket, userKey := Split("orphan")
	if bucket != "orphan" || userKey != "" {
		t.Errorf("Split(\"orphan\") = (%q, %q), want (\"orphan\", \"\")", bucket, userKey)
	}
}

// TestPrefix verifies the prefix contract.
// Asserts that Prefix(\"alpha\") = , want \"alpha/\.
func TestPrefix(t *testing.T) {
	t.Parallel()
	if got := Prefix("alpha"); got != "alpha/" {
		t.Errorf("Prefix(\"alpha\") = %q, want \"alpha/\"", got)
	}
}

// TestIsCopyOf verifies the bare key and its per-write paths are copies, and a
// different object sharing the prefix is not.
func TestIsCopyOf(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		path string
		want bool
	}{
		{"b/photo.jpg", true},
		{StorageKey("b/photo.jpg", id), true},
		{"b/photo.jpg!backup", false},
		{StorageKey("b/photo.jpg", "0123456789ABCDEF0123456789ABCDEF"), false},
		{StorageKey("b/photo.jpg", id[:31]), false},
		{StorageKey("b/photo.jpg!backup", id), false},
		{"b/photo.jpg2", false},
	}
	for _, c := range cases {
		if got := IsCopyOf("b/photo.jpg", c.path); got != c.want {
			t.Errorf("IsCopyOf(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
