// -------------------------------------------------------------------------------
// Bucket Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 bucket-level stub operations: HeadBucket, GetBucketLocation,
// and ListBuckets. Validates correct XML responses and auth enforcement.
// -------------------------------------------------------------------------------

package s3api

import (
	"io"
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestListBuckets verifies the list buckets contract.
// Asserts that expected 200, got.
func TestListBuckets(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")

	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xml := string(body)

	if !strings.Contains(xml, "<ListAllMyBucketsResult") {
		t.Errorf("missing ListAllMyBucketsResult element: %s", xml)
	}
	if !strings.Contains(xml, "<Name>mybucket</Name>") {
		t.Errorf("missing bucket name in response: %s", xml)
	}
}

// TestListBucketsNoAuth verifies the list buckets no auth contract.
// Asserts that expected 403, got.
func TestListBucketsNoAuth(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL + "/", nil)
	resp, err := ts.Client().Do(getReq) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// TestHeadBucket verifies the head bucket contract.
// Asserts that expected 200, got.
func TestHeadBucket(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp := doReq(t, ts, http.MethodHead, ts.URL+"/mybucket", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(body))
	}
}

// TestHeadBucketWrongBucket verifies the head bucket wrong bucket contract.
// Asserts that expected 403, got.
func TestHeadBucketWrongBucket(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp := doReq(t, ts, http.MethodHead, ts.URL+"/otherbucket", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// TestGetBucketLocation verifies the get bucket location contract.
// Asserts that expected 200, got.
func TestGetBucketLocation(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket?location", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xml := string(body)

	if !strings.Contains(xml, "<LocationConstraint") {
		t.Errorf("missing LocationConstraint element: %s", xml)
	}
}

// TestGetBucketLocationNoAuth verifies the get bucket location no auth contract.
// Asserts that expected 403, got.
func TestGetBucketLocationNoAuth(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL + "/mybucket?location", nil)
	resp, err := ts.Client().Do(getReq) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// TestGetBucketVersioning verifies the get bucket versioning contract.
// Asserts that expected 200, got.
func TestGetBucketVersioning(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket?versioning", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xml := string(body)

	if !strings.Contains(xml, "<VersioningConfiguration") {
		t.Errorf("missing VersioningConfiguration element: %s", xml)
	}
	// Empty VersioningConfiguration means versioning is not enabled
	if strings.Contains(xml, "<Status>") {
		t.Errorf("expected empty VersioningConfiguration (no Status element): %s", xml)
	}
}
