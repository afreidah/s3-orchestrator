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
	"io"
	"log/slog"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestServer_LoggerFallback pins the nil-safe behaviour of logger():
// tests construct *Server directly without setting log, and the helper
// must return slog.Default() so the s3api layer never dereferences a
// nil logger.
func TestServer_LoggerFallback(t *testing.T) {
	t.Parallel()
	if (&Server{}).logger() == nil {
		t.Fatal("logger() returned nil for zero-value Server")
	}
}

// TestServer_LoggerReturnsCustomLog covers the non-nil branch of
// logger(): a server constructed with a populated log field hands that
// exact logger back to callers rather than falling through to
// slog.Default().
func TestServer_LoggerReturnsCustomLog(t *testing.T) {
	t.Parallel()
	custom := slog.Default().With("scope", "test")
	srv := &Server{log: custom}
	if srv.logger() != custom {
		t.Fatal("logger() did not return the assigned log field")
	}
}

// TestNewServer_AssignsScopedLogger drives the production constructor
// so the log-field assignment and the component-scoping call execute
// under coverage.
func TestNewServer_AssignsScopedLogger(t *testing.T) {
	t.Parallel()
	mockStore := testutil.NewMockStore(t)
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Stores:    mockStore,
		Dashboard: mockStore,
		Metrics:   mockStore,
	})
	t.Cleanup(mgr.Close)
	srv := NewServer(mgr, 1024)
	if srv.log == nil {
		t.Fatal("NewServer left log field nil")
	}
}

// TestSetGetBucketAuth_RoundTrip verifies the set get bucket auth round trip path by exercising auth.NewBucketRegistry, srv.SetBucketAuth, srv.GetBucketAuth.
func TestSetGetBucketAuth_RoundTrip(t *testing.T) {
	t.Parallel()
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

// TestSetBucketAuth_ConcurrentAccess verifies the set bucket auth concurrent access path by exercising auth.NewBucketRegistry, srv.SetBucketAuth, wg.Add.
func TestSetBucketAuth_ConcurrentAccess(t *testing.T) {
	t.Parallel()
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
	for range goroutines {
		go func() {
			defer wg.Done()
			for range 100 {
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
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			for range 100 {
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

// TestBucketOnlyPUT_MethodNotAllowed verifies the bucket only put method not allowed contract.
// Asserts that status = , want 405.
func TestBucketOnlyPUT_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	// PUT to a bucket-only path (no key) should hit the default case
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
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
	req.Header.Set("X-Proxy-Token", "test-token")
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
	req.Header.Set("X-Proxy-Token", "test-token")
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
