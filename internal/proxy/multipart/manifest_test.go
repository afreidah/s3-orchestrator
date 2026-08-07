// -------------------------------------------------------------------------------
// Multipart Completion Manifest Tests
//
// Author: Alex Freidah
//
// Covers the protocol invariants completion enforces before it assembles
// anything: part-number range, ordering, duplicates, count, client ETags, and
// the minimum non-final part size. The shape and stored-comparison checks are
// exercised directly; the end-to-end assertion that a rejected manifest leaves
// the upload untouched lives in manager_fleet_test.go.
// -------------------------------------------------------------------------------

package multipart

import (
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// s3CodeOf extracts the S3 error code, failing the test when err is not a
// typed S3 error. Every rejection here must carry a code the transport can
// map, not a bare error.
func s3CodeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok {
		t.Fatalf("expected *core.S3Error, got %T: %v", err, err)
	}
	return s3err.Code
}

// TestValidateManifestShape covers every rejection that needs no store read,
// plus the ascending manifest that must pass.
func TestValidateManifestShape(t *testing.T) {
	t.Parallel()

	tooMany := make([]core.CompletePart, MaxPartCount+1)
	for i := range tooMany {
		tooMany[i] = core.CompletePart{PartNumber: i + 1}
	}

	for _, c := range []struct {
		name     string
		manifest []core.CompletePart
		wantCode string // empty means the manifest must be accepted
	}{
		{"ascending", []core.CompletePart{{PartNumber: 1}, {PartNumber: 2}, {PartNumber: 7}}, ""},
		{"single part", []core.CompletePart{{PartNumber: 1}}, ""},
		{"at the upper bound", []core.CompletePart{{PartNumber: MaxPartNumber}}, ""},
		{"empty", nil, "InvalidRequest"},
		{"part number zero", []core.CompletePart{{PartNumber: 0}}, "InvalidPart"},
		{"negative part number", []core.CompletePart{{PartNumber: -1}}, "InvalidPart"},
		{"above the upper bound", []core.CompletePart{{PartNumber: MaxPartNumber + 1}}, "InvalidPart"},
		{"duplicate", []core.CompletePart{{PartNumber: 1}, {PartNumber: 1}}, "InvalidPart"},
		{"descending", []core.CompletePart{{PartNumber: 2}, {PartNumber: 1}}, "InvalidPartOrder"},
		{"unordered", []core.CompletePart{{PartNumber: 1}, {PartNumber: 5}, {PartNumber: 3}}, "InvalidPartOrder"},
		{"too many parts", tooMany, "InvalidRequest"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateManifestShape(c.manifest)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("manifest should be accepted, got %v", err)
				}
				return
			}
			if got := s3CodeOf(t, err); got != c.wantCode {
				t.Errorf("code = %s, want %s", got, c.wantCode)
			}
		})
	}
}

// TestNormalizeETag pins the quoting forms a client may send. S3 SDKs differ
// on quoting, so the comparison has to see through it or every quoted
// manifest would be rejected as a mismatch.
func TestNormalizeETag(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{`"abc"`, "abc"},
		{"abc", "abc"},
		{`W/"abc"`, "abc"},
		{`  "abc"  `, "abc"},
		{"", ""},
	} {
		if got := normalizeETag(c.in); got != c.want {
			t.Errorf("normalizeETag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// storedParts is the two-part upload the stored-comparison cases run against:
// one 5 MiB part followed by a small final part, which is the shape S3 allows.
func storedParts() []core.MultipartPart {
	return []core.MultipartPart{
		{PartNumber: 1, ETag: "etag-one", SizeBytes: MinPartSizeBytes},
		{PartNumber: 2, ETag: "etag-two", SizeBytes: 10},
	}
}

// TestValidateManifestAgainstStored covers the checks that need the stored
// rows: existence, ETag match, and the non-final size floor.
func TestValidateManifestAgainstStored(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name           string
		manifest       []core.CompletePart
		stored         []core.MultipartPart
		enforceMinSize bool
		wantCode       string
	}{
		{
			name:     "matching etags",
			manifest: []core.CompletePart{{PartNumber: 1, ETag: "etag-one"}, {PartNumber: 2, ETag: "etag-two"}},
			stored:   storedParts(), enforceMinSize: true,
		},
		{
			name:     "quoted etags still match",
			manifest: []core.CompletePart{{PartNumber: 1, ETag: `"etag-one"`}, {PartNumber: 2, ETag: `"etag-two"`}},
			stored:   storedParts(), enforceMinSize: true,
		},
		{
			name:     "omitted etags skip the comparison",
			manifest: []core.CompletePart{{PartNumber: 1}, {PartNumber: 2}},
			stored:   storedParts(), enforceMinSize: true,
		},
		{
			name:     "mismatched etag",
			manifest: []core.CompletePart{{PartNumber: 1, ETag: "stale"}, {PartNumber: 2, ETag: "etag-two"}},
			stored:   storedParts(), enforceMinSize: true, wantCode: "InvalidPart",
		},
		{
			name:     "etag of a replaced part",
			manifest: []core.CompletePart{{PartNumber: 1, ETag: "etag-one"}},
			stored: []core.MultipartPart{
				{PartNumber: 1, ETag: "etag-one-v2", SizeBytes: MinPartSizeBytes},
			},
			enforceMinSize: true, wantCode: "InvalidPart",
		},
		{
			name:     "part never uploaded",
			manifest: []core.CompletePart{{PartNumber: 1, ETag: "etag-one"}, {PartNumber: 9, ETag: "nope"}},
			stored:   storedParts(), enforceMinSize: true, wantCode: "InvalidPart",
		},
		{
			name:     "undersized non-final part",
			manifest: []core.CompletePart{{PartNumber: 1}, {PartNumber: 2}},
			stored: []core.MultipartPart{
				{PartNumber: 1, ETag: "a", SizeBytes: 10},
				{PartNumber: 2, ETag: "b", SizeBytes: 10},
			},
			enforceMinSize: true, wantCode: "EntityTooSmall",
		},
		{
			name:     "undersized final part is allowed",
			manifest: []core.CompletePart{{PartNumber: 1}, {PartNumber: 2}},
			stored:   storedParts(), enforceMinSize: true,
		},
		{
			name:     "single undersized part is allowed",
			manifest: []core.CompletePart{{PartNumber: 1}},
			stored: []core.MultipartPart{
				{PartNumber: 1, ETag: "a", SizeBytes: 10},
			},
			enforceMinSize: true,
		},
		{
			name:     "undersized part passes when the floor is off",
			manifest: []core.CompletePart{{PartNumber: 1}, {PartNumber: 2}},
			stored: []core.MultipartPart{
				{PartNumber: 1, ETag: "a", SizeBytes: 10},
				{PartNumber: 2, ETag: "b", SizeBytes: 10},
			},
			enforceMinSize: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateManifestAgainstStored(c.manifest, c.stored, c.enforceMinSize)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("manifest should be accepted, got %v", err)
				}
				return
			}
			if got := s3CodeOf(t, err); got != c.wantCode {
				t.Errorf("code = %s, want %s", got, c.wantCode)
			}
		})
	}
}
