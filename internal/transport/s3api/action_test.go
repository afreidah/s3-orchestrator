// -------------------------------------------------------------------------------
// S3 API - Action Classification Tests
//
// Author: Alex Freidah
//
// The action set as a table. Classification is pure, so every operation the
// server implements is one row here rather than a request driven through a
// handler, and a method and query combination that names nothing is asserted to
// name nothing rather than falling into a neighbouring operation.
// -------------------------------------------------------------------------------

package s3api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// classify builds a request and names the action it asks for. An empty key
// selects the bucket vocabulary, matching how the router splits.
func classify(t *testing.T, method, target, key string, headers map[string]string) Action {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return Classify(r, key)
}

// copySource is the header that splits a write from a server-side copy.
var copySource = map[string]string{headerCopySource: "/other/key.txt"}

// -------------------------------------------------------------------------
// BUCKET
// -------------------------------------------------------------------------

// TestClassify_Bucket covers every bucket-level operation, including the two
// listing versions that differ only by a query value.
func TestClassify_Bucket(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		target string
		want   Action
	}{
		{"head bucket", http.MethodHead, "/photos", ActionHeadBucket},
		{"versioning", http.MethodGet, "/photos?versioning", ActionGetBucketVersioning},
		{"location", http.MethodGet, "/photos?location", ActionGetBucketLocation},
		{"list uploads", http.MethodGet, "/photos?uploads", ActionListMultipartUpload},
		{"list v2", http.MethodGet, "/photos?list-type=2", ActionListObjectsV2},
		{"list v1", http.MethodGet, "/photos", ActionListObjectsV1},
		{"list v1 with prefix", http.MethodGet, "/photos?prefix=a/", ActionListObjectsV1},
		{"batch delete", http.MethodPost, "/photos?delete", ActionDeleteObjects},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(t, tc.method, tc.target, "", nil); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassify_BucketUnknown verifies a method the bucket vocabulary does not
// accept names nothing, rather than falling into a listing. The router renders
// this as 405.
func TestClassify_BucketUnknown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ method, target string }{
		{http.MethodPut, "/photos"},
		{http.MethodDelete, "/photos"},
		{http.MethodPost, "/photos"},
		{http.MethodPatch, "/photos"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			if got := classify(t, tc.method, tc.target, "", nil); got != ActionUnknown {
				t.Errorf("Classify = %q, want no action", got)
			}
		})
	}
}

// -------------------------------------------------------------------------
// OBJECT
// -------------------------------------------------------------------------

// TestClassify_PlainObject covers the four non-subresource object operations
// plus the copy-source split, which is the one case where two operations share
// a method and differ only by a header.
func TestClassify_PlainObject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		want    Action
	}{
		{"get", http.MethodGet, nil, ActionGetObject},
		{"head", http.MethodHead, nil, ActionHeadObject},
		{"put", http.MethodPut, nil, ActionPutObject},
		{"put with copy source is a copy", http.MethodPut, copySource, ActionCopyObject},
		{"delete", http.MethodDelete, nil, ActionDeleteObject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classify(t, tc.method, "/photos/cat.jpg", "cat.jpg", tc.headers)
			if got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassify_Multipart covers the upload lifecycle, including the same
// copy-source split on a part that the plain object path makes on the object.
func TestClassify_Multipart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		want    Action
	}{
		{"create", http.MethodPost, "/photos/cat.jpg?uploads", nil, ActionCreateMultipartUpload},
		{"upload part", http.MethodPut, "/photos/cat.jpg?uploadId=u1&partNumber=1", nil, ActionUploadPart},
		{"upload part copy", http.MethodPut, "/photos/cat.jpg?uploadId=u1&partNumber=1", copySource, ActionUploadPartCopy},
		{"complete", http.MethodPost, "/photos/cat.jpg?uploadId=u1", nil, ActionCompleteMultipartUpload},
		{"abort", http.MethodDelete, "/photos/cat.jpg?uploadId=u1", nil, ActionAbortMultipartUpload},
		{"list parts", http.MethodGet, "/photos/cat.jpg?uploadId=u1", nil, ActionListParts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(t, tc.method, tc.target, "cat.jpg", tc.headers); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassify_Tagging covers the three ?tagging operations and pins the
// ordering that matters: a tagging request carries neither uploads nor
// uploadId, so classifying it after the multipart split would name it the plain
// object operation its method implies and reach the object itself.
func TestClassify_Tagging(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method string
		want   Action
	}{
		{http.MethodGet, ActionGetObjectTagging},
		{http.MethodPut, ActionPutObjectTagging},
		{http.MethodDelete, ActionDeleteObjectTagging},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			got := classify(t, tc.method, "/photos/cat.jpg?tagging", "cat.jpg", nil)
			if got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassify_ObjectUnknown verifies a method no object vocabulary accepts
// names nothing, at each of the three splits.
func TestClassify_ObjectUnknown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, method, target string }{
		{"plain", http.MethodPost, "/photos/cat.jpg"},
		{"plain patch", http.MethodPatch, "/photos/cat.jpg"},
		{"multipart", http.MethodPatch, "/photos/cat.jpg?uploadId=u1"},
		{"tagging", http.MethodPost, "/photos/cat.jpg?tagging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(t, tc.method, tc.target, "cat.jpg", nil); got != ActionUnknown {
				t.Errorf("Classify = %q, want no action", got)
			}
		})
	}
}

// -------------------------------------------------------------------------
// UNSUPPORTED SUBRESOURCES
// -------------------------------------------------------------------------

// TestClassify_UnsupportedSubresource verifies a query naming a subresource
// this server does not implement is classified as such rather than falling
// through. Falling through is the dangerous case: on a bucket it answers a
// listing to a caller that asked for a policy, and on an object it would run
// PutObject or DeleteObject against the key.
func TestClassify_UnsupportedSubresource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		target string
		key    string
	}{
		{"bucket policy", http.MethodGet, "/photos?policy", ""},
		{"bucket lifecycle", http.MethodGet, "/photos?lifecycle", ""},
		{"bucket versions", http.MethodGet, "/photos?versions", ""},
		{"object acl", http.MethodGet, "/photos/cat.jpg?acl", "cat.jpg"},
		{"object retention", http.MethodPut, "/photos/cat.jpg?retention", "cat.jpg"},
		{"object legal hold", http.MethodPut, "/photos/cat.jpg?legal-hold", "cat.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classify(t, tc.method, tc.target, tc.key, nil)
			if got != ActionUnsupportedSubresource {
				t.Errorf("Classify = %q, want %q", got, ActionUnsupportedSubresource)
			}
		})
	}
}

// -------------------------------------------------------------------------
// COVERAGE
// -------------------------------------------------------------------------

// TestClassify_EveryActionIsReachable pins that no constant is declared without
// a request that produces it. An action nothing classifies to is one an
// authorization rule could be written against and never evaluated.
func TestClassify_EveryActionIsReachable(t *testing.T) {
	t.Parallel()

	reached := map[Action]bool{}
	for _, tc := range []struct {
		method  string
		target  string
		key     string
		headers map[string]string
	}{
		{http.MethodHead, "/photos", "", nil},
		{http.MethodGet, "/photos?versioning", "", nil},
		{http.MethodGet, "/photos?location", "", nil},
		{http.MethodGet, "/photos?uploads", "", nil},
		{http.MethodGet, "/photos?list-type=2", "", nil},
		{http.MethodGet, "/photos", "", nil},
		{http.MethodPost, "/photos?delete", "", nil},
		{http.MethodGet, "/photos/cat.jpg", "cat.jpg", nil},
		{http.MethodHead, "/photos/cat.jpg", "cat.jpg", nil},
		{http.MethodPut, "/photos/cat.jpg", "cat.jpg", nil},
		{http.MethodPut, "/photos/cat.jpg", "cat.jpg", copySource},
		{http.MethodDelete, "/photos/cat.jpg", "cat.jpg", nil},
		{http.MethodPost, "/photos/cat.jpg?uploads", "cat.jpg", nil},
		{http.MethodPut, "/photos/cat.jpg?uploadId=u1", "cat.jpg", nil},
		{http.MethodPut, "/photos/cat.jpg?uploadId=u1", "cat.jpg", copySource},
		{http.MethodPost, "/photos/cat.jpg?uploadId=u1", "cat.jpg", nil},
		{http.MethodDelete, "/photos/cat.jpg?uploadId=u1", "cat.jpg", nil},
		{http.MethodGet, "/photos/cat.jpg?uploadId=u1", "cat.jpg", nil},
		{http.MethodGet, "/photos/cat.jpg?tagging", "cat.jpg", nil},
		{http.MethodPut, "/photos/cat.jpg?tagging", "cat.jpg", nil},
		{http.MethodDelete, "/photos/cat.jpg?tagging", "cat.jpg", nil},
		{http.MethodGet, "/photos?policy", "", nil},
	} {
		reached[classify(t, tc.method, tc.target, tc.key, tc.headers)] = true
	}

	for _, act := range []Action{
		ActionHeadBucket, ActionGetBucketVersioning, ActionGetBucketLocation,
		ActionListMultipartUpload, ActionListObjectsV1, ActionListObjectsV2,
		ActionDeleteObjects, ActionGetObject, ActionHeadObject, ActionPutObject,
		ActionCopyObject, ActionDeleteObject, ActionCreateMultipartUpload,
		ActionUploadPart, ActionUploadPartCopy, ActionCompleteMultipartUpload,
		ActionAbortMultipartUpload, ActionListParts, ActionGetObjectTagging,
		ActionPutObjectTagging, ActionDeleteObjectTagging,
		ActionUnsupportedSubresource,
	} {
		if !reached[act] {
			t.Errorf("no request classifies to %q", act)
		}
	}
}

// -------------------------------------------------------------------------
// REQUIRED PERMISSIONS
// -------------------------------------------------------------------------

// TestRequiredPermissions covers what each action asks a grant to carry,
// including the three judgement calls the mapping makes.
func TestRequiredPermissions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		act  Action
		want core.PermissionSet
	}{
		{ActionHeadBucket, core.PermListBuckets},
		{ActionGetBucketLocation, core.PermListBuckets},
		{ActionListObjectsV2, core.PermList},
		{ActionListParts, core.PermList},
		{ActionGetObject, core.PermRead},
		{ActionHeadObject, core.PermRead},
		{ActionPutObject, core.PermWrite},
		{ActionDeleteObject, core.PermDelete},
		{ActionDeleteObjects, core.PermDelete},
		{ActionGetObjectTagging, core.PermTags},
		{ActionPutObjectTagging, core.PermTags},

		// An abandoned upload is the client cleaning up after itself. A
		// writer that cannot abort leaks parts it has no other way to remove.
		{ActionAbortMultipartUpload, core.PermWrite},

		// A copy reads the source it names as well as writing its destination.
		{ActionCopyObject, core.PermRead | core.PermWrite},
		{ActionUploadPartCopy, core.PermRead | core.PermWrite},

		// Refused before reaching an object, so gating them would answer 403
		// where the server means 501 or 405.
		{ActionUnsupportedSubresource, 0},
		{ActionUnknown, 0},
	} {
		t.Run(string(tc.act), func(t *testing.T) {
			t.Parallel()
			if got := RequiredPermissions(tc.act); got != tc.want {
				t.Errorf("RequiredPermissions(%q) = %q, want %q", tc.act, got, tc.want)
			}
		})
	}
}

// TestRequiredPermissions_EveryActionIsMapped pins that no operation reaches a
// handler without a permission decision. An action absent from the map is
// authorized by any grant, which is the failure mode that would not announce
// itself.
func TestRequiredPermissions_EveryActionIsMapped(t *testing.T) {
	t.Parallel()

	for _, act := range []Action{
		ActionHeadBucket, ActionGetBucketVersioning, ActionGetBucketLocation,
		ActionListMultipartUpload, ActionListObjectsV1, ActionListObjectsV2,
		ActionDeleteObjects, ActionGetObject, ActionHeadObject, ActionPutObject,
		ActionCopyObject, ActionDeleteObject, ActionCreateMultipartUpload,
		ActionUploadPart, ActionUploadPartCopy, ActionCompleteMultipartUpload,
		ActionAbortMultipartUpload, ActionListParts, ActionGetObjectTagging,
		ActionPutObjectTagging, ActionDeleteObjectTagging,
	} {
		if RequiredPermissions(act) == 0 {
			t.Errorf("%q requires no permission, so any grant authorizes it", act)
		}
	}
}

// TestRequiredPermissions_ReadOnlyGrantRefusesEveryWrite walks the whole action
// set against a read-only grant, which is the arrangement the feature exists
// for. Anything needing write or delete has to be refused.
func TestRequiredPermissions_ReadOnlyGrantRefusesEveryWrite(t *testing.T) {
	t.Parallel()

	readOnly := core.PermListBuckets | core.PermList | core.PermRead | core.PermTags
	mutating := map[Action]bool{
		ActionPutObject: true, ActionCopyObject: true, ActionDeleteObject: true,
		ActionDeleteObjects: true, ActionCreateMultipartUpload: true,
		ActionUploadPart: true, ActionUploadPartCopy: true,
		ActionCompleteMultipartUpload: true, ActionAbortMultipartUpload: true,
	}

	for act := range requiredPermissions {
		allowed := readOnly.Has(RequiredPermissions(act))
		if mutating[act] && allowed {
			t.Errorf("%q is allowed by a read-only grant", act)
		}
		if !mutating[act] && !allowed {
			t.Errorf("%q is refused by a read-only grant", act)
		}
	}
}
