// -------------------------------------------------------------------------------
// HTTP Server Tests
//
// Author: Alex Freidah
//
// Tests for S3-compatible HTTP server setup, routing, middleware chain, and
// graceful shutdown behavior.
// -------------------------------------------------------------------------------

package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestSetGetBucketAuth_RoundTrip(t *testing.T) {
	srv := &Server{}

	buckets := []config.BucketConfig{
		{Name: "b1", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AKID1", SecretAccessKey: "secret1"},
		}},
	}
	br := auth.NewBucketRegistry(buckets)
	srv.SetBucketAuth(br)

	got := srv.GetBucketAuth()
	if got != br {
		t.Error("GetBucketAuth should return the same registry that was set")
	}
}

func TestSetBucketAuth_ConcurrentAccess(t *testing.T) {
	srv := &Server{}

	// Set an initial registry
	initial := auth.NewBucketRegistry([]config.BucketConfig{
		{Name: "init", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AKID0", SecretAccessKey: "secret0"},
		}},
	})
	srv.SetBucketAuth(initial)

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent readers
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				br := srv.GetBucketAuth()
				if br == nil {
					t.Error("GetBucketAuth returned nil during concurrent access")
					return
				}
			}
		}()
	}

	// Concurrent writers
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				br := auth.NewBucketRegistry([]config.BucketConfig{
					{Name: "b", Credentials: []config.CredentialConfig{
						{AccessKeyID: "AKID", SecretAccessKey: "secret"},
					}},
				})
				srv.SetBucketAuth(br)
			}
		}(i)
	}

	wg.Wait()
	// Test passes if no race detector violations
}

// -------------------------------------------------------------------------
// Routing: untested code paths in ServeHTTP
// -------------------------------------------------------------------------

func TestBucketOnlyPUT_MethodNotAllowed(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// PUT to a bucket-only path (no key) should hit the default case
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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

func TestMultipartUpload_UnsupportedMethod(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// PATCH to a key path with uploadId should hit the multipart default case
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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

func TestInvalidPath_Returns400(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// POST to "/" — not intercepted as ListBuckets, so parsePath returns
	// false for the empty path and the server returns 400.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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
