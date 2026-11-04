// -------------------------------------------------------------------------------
// HTTP Server Tests
//
// Author: Alex Freidah
//
// Tests for S3-compatible HTTP server setup, routing, middleware chain, and
// graceful shutdown behavior.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestNewServer_AssignsScopedLogger drives the production constructor
// so the log-field assignment and the component-scoping call execute
// under coverage.
func TestNewServer_AssignsScopedLogger(t *testing.T) {
	t.Parallel()
	mockStore := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := proxytest.New(t, mockStore, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{Metrics: mockStore}),
	})
	srv := NewServer(st.Objects, st.Multipart, auth.NewLiveRegistry(nil), 1024)
	if srv.log == nil {
		t.Fatal("NewServer left log field nil")
	}
}

// TestServer_FollowsTheRegistrySwap verifies a credential added by swapping the
// live registry is honored on the next request, with no handle on the server.
func TestServer_FollowsTheRegistrySwap(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	registry := auth.NewLiveRegistry(mustBucketRegistry(t, nil))
	ts := httptest.NewServer(NewServer(NewMockObjectOps(ctrl), NewMockMultipartOps(ctrl), registry, 0))
	t.Cleanup(ts.Close)

	headBucket := func() int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodHead, ts.URL+"/mybucket", nil)
		signRequest(t, req)
		resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := headBucket(); got != http.StatusForbidden {
		t.Fatalf("before the swap: status = %d, want 403", got)
	}
	registry.Store(mustBucketRegistry(t, []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{testCredential()}},
	}))
	if got := headBucket(); got != http.StatusOK {
		t.Fatalf("after the swap: status = %d, want 200", got)
	}
}

// -------------------------------------------------------------------------
// Routing: untested code paths in ServeHTTP
// -------------------------------------------------------------------------

// TestBucketOnlyPUT_MethodNotAllowed verifies the bucket only put method not allowed contract.
// Asserts that status = , want 405.
func TestBucketOnlyPUT_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	// PUT to a bucket-only path (no key) should hit the default case
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/", nil)
	signRequest(t, req)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MethodNotAllowed") {
		t.Error("response should contain MethodNotAllowed error code")
	}
}

// TestMultipartUpload_UnsupportedMethod verifies the multipart upload unsupported method contract.
// Asserts that status = , want 405.
func TestMultipartUpload_UnsupportedMethod(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	// PATCH to a key path with uploadId should hit the multipart default case
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPatch, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	signRequest(t, req)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MethodNotAllowed") {
		t.Error("response should contain MethodNotAllowed error code")
	}
}

// TestInvalidPath_Returns400 verifies the invalid path returns400 contract.
// Asserts that status = , want 400.
func TestInvalidPath_Returns400(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	// POST to "/"  -  not intercepted as ListBuckets, so parsePath returns
	// false for the empty path and the server returns 400.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/", nil)
	signRequest(t, req)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InvalidRequest") {
		t.Error("response should contain InvalidRequest error code")
	}
}

// newOpsServer builds a Server over mocked ObjectOps/MultipartOps, with no
// proxy stack, store, or backend behind it. Use it for the handler
// behaviour that depends only on what the ops layer returns - status
// mapping, header handling, response shape - so the test states the one
// call it cares about instead of steering a whole fleet into that state.
func newOpsServer(t *testing.T) (*httptest.Server, *MockObjectOps, *MockMultipartOps) {
	t.Helper()
	ctrl := gomock.NewController(t)
	objects, multipart := NewMockObjectOps(ctrl), NewMockMultipartOps(ctrl)

	ts := httptest.NewServer(NewServer(objects, multipart, mustLiveRegistry(t, []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{testCredential()}},
	}), 10*1024*1024))
	t.Cleanup(ts.Close)
	return ts, objects, multipart
}

// mustBucketRegistry builds a registry from config the test controls, failing
// the test if that config turns out to be ambiguous.
func mustBucketRegistry(tb testing.TB, buckets []config.BucketConfig) *auth.BucketRegistry {
	tb.Helper()
	v := provisioning.Merge(buckets, config.AuthConfig{}, &provisioning.Snapshot{})
	br, err := auth.NewBucketRegistry(&v)
	if err != nil {
		tb.Fatalf("NewBucketRegistry: %v", err)
	}
	return br
}

// mustLiveRegistry is mustBucketRegistry as the holder a server reads.
func mustLiveRegistry(tb testing.TB, buckets []config.BucketConfig) *auth.LiveRegistry {
	tb.Helper()
	return auth.NewLiveRegistry(mustBucketRegistry(tb, buckets))
}
