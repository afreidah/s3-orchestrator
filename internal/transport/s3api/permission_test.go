// -------------------------------------------------------------------------------
// S3 API - Grant Permission Enforcement Tests
//
// Author: Alex Freidah
//
// Real requests against a server whose caller holds a narrowed grant. The
// mapping from action to permission is a table tested elsewhere; what these
// pin is that the check actually sits in the request path, refuses before the
// handler runs, and answers the same 403 whichever permission is missing.
// -------------------------------------------------------------------------------

package s3api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// grantRegistry builds a registry holding one stored user, reached by the test
// proxy token, whose grant on the test bucket carries exactly perms.
//
// Built from a store snapshot rather than config because a config-declared
// credential always carries every permission; narrowing one is only expressible
// as a stored grant.
func grantRegistry(tb testing.TB, perms core.PermissionSet) *auth.BucketRegistry {
	tb.Helper()
	v := provisioning.Merge(nil, &provisioning.Snapshot{
		Buckets: []core.Bucket{{Name: "mybucket"}},
		Users:   []core.User{{ID: "u1", Name: "narrow"}},
		Credentials: []core.Credential{
			{AccessKeyID: "AK", UserID: "u1", Secret: "SK"},
		},
		Grants: []core.Grant{
			{UserID: "u1", Resource: core.BucketResource("mybucket"), Permissions: perms},
		},
	})
	// The proxy token is the simplest credential to sign nothing with, and
	// authorization is what these exercise rather than signature verification.
	v.Credentials = append(v.Credentials, provisioning.Credential{
		UserID: "u1", Token: "test-token", Source: provisioning.SourceStore,
	})
	br, err := auth.NewBucketRegistry(&v)
	if err != nil {
		tb.Fatalf("NewBucketRegistry: %v", err)
	}
	return br
}

// serveWithGrant drives one request against a server whose caller holds perms,
// and reports the status.
func serveWithGrant(t *testing.T, perms core.PermissionSet, method, target string, body string, opts ...func(*storetest.MockMetadataStore)) int {
	t.Helper()
	ts, _, _ := newTestServer(t, opts...)
	// Replace the registry the fixture installed with one carrying a narrowed
	// grant. The atomic swap is what a provisioning change performs, so this is
	// the same path a live narrowing takes.
	ts.Config.Handler.(*Server).SetBucketAuth(grantRegistry(t, perms))

	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+target, bodyReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// bodyReader returns an untyped nil for an empty body. Returning a typed nil
// pointer would hand net/http a non-nil io.Reader wrapping one, which it
// dereferences while sizing the request.
func bodyReader(body string) io.Reader {
	if body == "" {
		return nil
	}
	return strings.NewReader(body)
}

// seedEmptyListing gives the store a real empty page to answer a listing with.
// The permissive default returns a nil result, which the handler dereferences,
// so an authorized list needs this to reach a status at all.
func seedEmptyListing(m *storetest.MockMetadataStore) {
	m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.ListObjectsResult{}, nil).AnyTimes()
}

// readOnly is the grant a client that may look but not touch holds.
var readOnly = core.PermListBuckets | core.PermList | core.PermRead | core.PermTags

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestPermissions_ReadOnlyGrantRefusesWrites is what the feature exists for: a
// caller holding read but not write is served its reads and refused its writes.
func TestPermissions_ReadOnlyGrantRefusesWrites(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		{"put is refused", http.MethodPut, "/mybucket/x.txt", "payload", http.StatusForbidden},
		{"delete is refused", http.MethodDelete, "/mybucket/x.txt", "", http.StatusForbidden},
		{"batch delete is refused", http.MethodPost, "/mybucket?delete", "<Delete></Delete>", http.StatusForbidden},
		{"create upload is refused", http.MethodPost, "/mybucket/x.txt?uploads", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := serveWithGrant(t, readOnly, tc.method, tc.target, tc.body); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPermissions_ReadOnlyGrantServesReads verifies the same grant is not
// refused what it does carry. A check that denied everything would pass the
// test above for the wrong reason.
func TestPermissions_ReadOnlyGrantServesReads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"head bucket", http.MethodHead, "/mybucket"},
		{"list objects", http.MethodGet, "/mybucket?list-type=2"},
		{"get object", http.MethodGet, "/mybucket/absent.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if serveWithGrant(t, readOnly, tc.method, tc.target, "", seedEmptyListing) == http.StatusForbidden {
				t.Errorf("status = 403, want the request to be authorized")
			}
		})
	}
}

// TestPermissions_WriteOnlyGrantRefusesReads covers the inverse arrangement, a
// drop target that may not read back what it wrote.
func TestPermissions_WriteOnlyGrantRefusesReads(t *testing.T) {
	t.Parallel()

	writeOnly := core.PermListBuckets | core.PermWrite

	if got := serveWithGrant(t, writeOnly, http.MethodGet, "/mybucket/x.txt", ""); got != http.StatusForbidden {
		t.Errorf("get status = %d, want 403", got)
	}
	if got := serveWithGrant(t, writeOnly, http.MethodGet, "/mybucket?list-type=2", ""); got != http.StatusForbidden {
		t.Errorf("list status = %d, want 403", got)
	}
	if serveWithGrant(t, writeOnly, http.MethodPut, "/mybucket/x.txt", "payload") == http.StatusForbidden {
		t.Error("put status = 403, want the write to be authorized")
	}
}

// TestPermissions_FullGrantIsUnchanged verifies a grant carrying everything
// behaves as it did before permissions existed, which is what every grant
// written before this migration holds.
func TestPermissions_FullGrantIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPut, "/mybucket/x.txt", "payload"},
		{http.MethodGet, "/mybucket?list-type=2", ""},
		{http.MethodDelete, "/mybucket/x.txt", ""},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			if serveWithGrant(t, core.PermAll, tc.method, tc.target, tc.body, seedEmptyListing) == http.StatusForbidden {
				t.Errorf("%s %s = 403 under a full grant", tc.method, tc.target)
			}
		})
	}
}

// TestPermissions_RefusedBeforeTheHandler pins that the check runs ahead of
// dispatch. A refused PUT that had already reached the handler would have
// stored the object it was denied.
func TestPermissions_RefusedBeforeTheHandler(t *testing.T) {
	t.Parallel()

	ts, _, backend := newTestServer(t)
	ts.Config.Handler.(*Server).SetBucketAuth(grantRegistry(t, readOnly))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		ts.URL+"/mybucket/denied.txt", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := backend.GetObject(t.Context(), "mybucket/denied.txt", ""); err == nil {
		t.Error("the refused write reached the backend")
	}
}

// TestPermissions_UnsupportedSubresourceStillAnswers501 verifies an operation
// needing no permission is not turned into a 403 by the check. Answering 403
// where the server means 501 would tell a caller its grant is wrong when the
// server simply does not implement what it asked for.
func TestPermissions_UnsupportedSubresourceStillAnswers501(t *testing.T) {
	t.Parallel()

	// Nothing but the bucket itself, so any permission check would refuse.
	bare := core.PermListBuckets
	if got := serveWithGrant(t, bare, http.MethodGet, "/mybucket?policy", ""); got != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", got)
	}
}
