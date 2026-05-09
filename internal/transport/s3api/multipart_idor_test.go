// -------------------------------------------------------------------------------
// Multipart IDOR Regression Tests
//
// Author: Alex Freidah
//
// Pins the cross-bucket isolation guarantee for the multipart endpoints.
// UploadPart, CompleteMultipartUpload, and AbortMultipartUpload all accept an
// uploadId from the query string. Without per-request validation that the
// upload's stored ObjectKey shares the URL's bucket prefix, an authenticated
// caller for bucket A can manipulate a multipart upload that belongs to
// bucket B (write parts into it, abort it, complete it). These tests pin the
// expected rejection so the bug cannot regress unnoticed.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
)

// -------------------------------------------------------------------------
// FIXTURE
// -------------------------------------------------------------------------

// twoBucketServer constructs a test server with two distinct buckets, each
// with its own credential token, so cross-bucket IDOR scenarios can be
// driven by varying the X-Proxy-Token header. The mock store's
// GetMultipartUpload always returns mu, simulating a multipart upload that
// physically lives under bucket-b.
func twoBucketServer(t *testing.T, mu *core.MultipartUpload) (*httptest.Server, *testutil.MockStore) {
	t.Helper()

	backend := newServerMockBackend()
	mockStore := &testutil.MockStore{
		GetBackendResp:   "b1",
		GetMultipartResp: mu,
	}

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": backend},
		Stores:          proxytest.StoresFromMock(mockStore),
		Dashboard:       mockStore,
		Metrics:         mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	srv := &Server{
		Manager:       mgr,
		MaxObjectSize: 10 * 1024 * 1024,
	}
	srv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{
		{Name: "bucket-a", Credentials: []config.CredentialConfig{{Token: "token-a"}}},
		{Name: "bucket-b", Credentials: []config.CredentialConfig{{Token: "token-b"}}},
	}))

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, mockStore
}

// doMultipartReq sends a request with the given bucket-A token and returns
// the response. Centralised so each test focuses on its specific URL/body.
func doMultipartReq(t *testing.T, method, url, token string, body io.Reader, contentLength int64) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Proxy-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = contentLength
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL is localhost
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// -------------------------------------------------------------------------
// REGRESSION TESTS
// -------------------------------------------------------------------------

// TestMultipartIDOR_UploadPart_RejectsCrossBucket asserts that a caller
// holding valid creds for bucket-a cannot upload a part to a multipart
// upload whose ObjectKey belongs to bucket-b. Today the request succeeds
// (200 OK) because the handler ignores the URL bucket; the assertion
// pins the post-fix behaviour (404 NoSuchUpload) so the regression is
// detected as soon as it is reintroduced.
func TestMultipartIDOR_UploadPart_RejectsCrossBucket(t *testing.T) {
	t.Parallel()

	mu := &core.MultipartUpload{
		UploadID:    "victim-upload",
		ObjectKey:   "bucket-b/secret-data",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}
	ts, _ := twoBucketServer(t, mu)

	body := strings.NewReader("attacker-bytes")
	resp := doMultipartReq(t,
		http.MethodPut,
		ts.URL+"/bucket-a/anything?uploadId=victim-upload&partNumber=1",
		"token-a",
		body, int64(len("attacker-bytes")),
	)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-bucket UploadPart accepted: status=%d body=%s", resp.StatusCode, respBody)
	}
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 NoSuchUpload. body: %s", resp.StatusCode, respBody)
	}
}

// TestMultipartIDOR_CompleteMultipart_RejectsCrossBucket asserts that
// CompleteMultipartUpload refuses to finalise an upload whose stored
// ObjectKey does not match the URL's bucket/key pair. Today the handler
// completes the upload at the original storage destination and returns a
// success response that lies about which bucket/key was written.
func TestMultipartIDOR_CompleteMultipart_RejectsCrossBucket(t *testing.T) {
	t.Parallel()

	mu := &core.MultipartUpload{
		UploadID:    "victim-upload",
		ObjectKey:   "bucket-b/secret-data",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}
	ts, _ := twoBucketServer(t, mu)

	body, err := xml.Marshal(completeMultipartUploadRequest{
		Parts: []completePart{{PartNumber: 1, ETag: `"deadbeef"`}},
	})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	resp := doMultipartReq(t,
		http.MethodPost,
		ts.URL+"/bucket-a/anything?uploadId=victim-upload",
		"token-a",
		strings.NewReader(string(body)), int64(len(body)),
	)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-bucket CompleteMultipartUpload accepted: status=%d body=%s", resp.StatusCode, respBody)
	}
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 NoSuchUpload. body: %s", resp.StatusCode, respBody)
	}
}

// TestMultipartIDOR_ListParts_RejectsCrossBucket asserts that ListParts
// refuses to enumerate parts of an upload whose ObjectKey belongs to a
// different bucket. Without this scope check a caller for bucket-a could
// enumerate a victim's parts (counts, sizes, ETags) before any complete
// or abort.
func TestMultipartIDOR_ListParts_RejectsCrossBucket(t *testing.T) {
	t.Parallel()

	mu := &core.MultipartUpload{
		UploadID:    "victim-upload",
		ObjectKey:   "bucket-b/secret-data",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}
	ts, _ := twoBucketServer(t, mu)

	resp := doMultipartReq(t,
		http.MethodGet,
		ts.URL+"/bucket-a/anything?uploadId=victim-upload",
		"token-a",
		nil, 0,
	)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-bucket ListParts accepted: status=%d body=%s", resp.StatusCode, respBody)
	}
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 NoSuchUpload. body: %s", resp.StatusCode, respBody)
	}
}

// TestMultipartIDOR_AbortMultipart_RejectsCrossBucket asserts that
// AbortMultipartUpload refuses to delete an upload whose stored ObjectKey
// belongs to a different bucket. Without the bucket-scope check the
// handler accepts the abort, allowing a caller for bucket-a to wipe an
// in-flight upload that physically lives in bucket-b.
func TestMultipartIDOR_AbortMultipart_RejectsCrossBucket(t *testing.T) {
	t.Parallel()

	mu := &core.MultipartUpload{
		UploadID:    "victim-upload",
		ObjectKey:   "bucket-b/secret-data",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}
	ts, _ := twoBucketServer(t, mu)

	resp := doMultipartReq(t,
		http.MethodDelete,
		ts.URL+"/bucket-a/anything?uploadId=victim-upload",
		"token-a",
		nil, 0,
	)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		t.Fatalf("cross-bucket AbortMultipartUpload accepted: status=%d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 NoSuchUpload. body: %s", resp.StatusCode, respBody)
	}
}
