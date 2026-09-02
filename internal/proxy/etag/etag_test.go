// -------------------------------------------------------------------------------
// ETag Tests
//
// Author: Alex Freidah
//
// The values here are the ones S3 itself produces, which is the whole point of
// the package: a client that computes an ETag locally has to arrive at the
// same string. The multipart vectors are the documented algorithm - MD5 over
// the concatenated binary part digests, then the part count - checked against
// digests computed here rather than against a value this package generated.
// -------------------------------------------------------------------------------

package etag

import (
	"crypto/md5" //nolint:gosec // G501: the algorithm under test
	"encoding/hex"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// md5Hex is the digest a client would compute over its own bytes.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // G401: see above
	return hex.EncodeToString(sum[:])
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestSingle_MatchesS3sWholeObjectForm pins the single-part shape: the hex
// digest, quoted.
func TestSingle_MatchesS3sWholeObjectForm(t *testing.T) {
	t.Parallel()
	digest := md5Hex("hello world")
	if got, want := Single(digest), `"`+digest+`"`; got != want {
		t.Errorf("Single = %q, want %q", got, want)
	}
}

// TestSingle_EmptyDigestStaysEmpty covers the caller that never hashed: an
// empty digest must not become a pair of quotes, which a client would compare
// against and fail.
func TestSingle_EmptyDigestStaysEmpty(t *testing.T) {
	t.Parallel()
	if got := Single(""); got != "" {
		t.Errorf("Single(\"\") = %q, want empty", got)
	}
}

// TestHex_NilHasherIsEmpty covers the optional-hasher call sites.
func TestHex_NilHasherIsEmpty(t *testing.T) {
	t.Parallel()
	if got := Hex(nil); got != "" {
		t.Errorf("Hex(nil) = %q, want empty", got)
	}
}

// TestMultipart_MatchesTheDocumentedAlgorithm computes the expected value the
// way AWS documents it, independently of the implementation.
func TestMultipart_MatchesTheDocumentedAlgorithm(t *testing.T) {
	t.Parallel()
	parts := []string{md5Hex("part one"), md5Hex("part two"), md5Hex("part three")}

	sum := md5.New() //nolint:gosec // G401: the algorithm under test
	for _, p := range parts {
		raw, err := hex.DecodeString(p)
		if err != nil {
			t.Fatalf("decode %q: %v", p, err)
		}
		sum.Write(raw)
	}
	want := `"` + hex.EncodeToString(sum.Sum(nil)) + `-3"`

	got, err := Multipart(parts)
	if err != nil {
		t.Fatalf("Multipart: %v", err)
	}
	if got != want {
		t.Errorf("Multipart = %q, want %q", got, want)
	}
}

// TestMultipart_SinglePartStillCarriesTheSuffix pins that a one-part upload
// reports "-1" rather than the whole-object form: S3 distinguishes them, and a
// client uses the suffix to tell that the object was uploaded in parts.
func TestMultipart_SinglePartStillCarriesTheSuffix(t *testing.T) {
	t.Parallel()
	got, err := Multipart([]string{md5Hex("only")})
	if err != nil {
		t.Fatalf("Multipart: %v", err)
	}
	if len(got) < 3 || got[len(got)-3:] != `-1"` {
		t.Errorf("Multipart = %q, want a -1 suffix", got)
	}
}

// TestMultipart_MissingDigestYieldsNoComposite covers the upload whose parts
// predate per-part digests: a composite over a subset would be a value no
// client could reproduce, so none is produced and the caller falls back.
func TestMultipart_MissingDigestYieldsNoComposite(t *testing.T) {
	t.Parallel()
	got, err := Multipart([]string{md5Hex("a"), "", md5Hex("c")})
	if err != nil {
		t.Fatalf("Multipart: %v", err)
	}
	if got != "" {
		t.Errorf("Multipart = %q, want empty when a part digest is missing", got)
	}
}

// TestMultipart_NoPartsYieldsNoComposite covers the degenerate list.
func TestMultipart_NoPartsYieldsNoComposite(t *testing.T) {
	t.Parallel()
	got, err := Multipart(nil)
	if err != nil {
		t.Fatalf("Multipart: %v", err)
	}
	if got != "" {
		t.Errorf("Multipart = %q, want empty", got)
	}
}

// TestMultipart_NonHexDigestErrors pins that a corrupt stored digest is
// reported rather than silently producing a composite over partial input.
func TestMultipart_NonHexDigestErrors(t *testing.T) {
	t.Parallel()
	if _, err := Multipart([]string{"zzzz"}); err == nil {
		t.Error("expected an error for a non-hex part digest")
	}
}

// TestNormalize_StripsQuotesAndWeakPrefix covers both forms a client sends.
func TestNormalize_StripsQuotesAndWeakPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{`"abc"`, "abc"},
		{`W/"abc"`, "abc"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
