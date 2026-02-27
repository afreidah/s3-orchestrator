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
