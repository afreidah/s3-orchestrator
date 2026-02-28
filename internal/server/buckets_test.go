// -------------------------------------------------------------------------------
// Bucket Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 bucket-level stub operations: HeadBucket, GetBucketLocation,
// and ListBuckets. Validates correct XML responses and auth enforcement.
// -------------------------------------------------------------------------------

package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListBuckets(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")

	resp, err := http.DefaultClient.Do(req)
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

func TestListBucketsNoAuth(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestHeadBucket(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(body))
	}
}

func TestHeadBucketWrongBucket(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp := doReq(t, http.MethodHead, ts.URL+"/otherbucket", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestGetBucketLocation(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket?location", nil)
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

func TestGetBucketLocationNoAuth(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/mybucket?location")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
