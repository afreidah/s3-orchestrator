// -------------------------------------------------------------------------------
// Integration Tests - Object Data Cache
//
// Author: Alex Freidah
//
// Integration tests for the in-memory object data cache. Verifies cache hits,
// misses, invalidation on writes, and correct data round-tripping through the
// cache layer with real MinIO backends and PostgreSQL.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/server"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// cacheTestEnv holds the components for cache integration tests.
type cacheTestEnv struct {
	client *s3.Client
	cache  *objcache.MemoryCache
}

// setupCacheEnv creates a proxy with object data caching enabled and returns
// the S3 client and cache instance for inspection.
func setupCacheEnv(t *testing.T) *cacheTestEnv {
	t.Helper()
	resetState(t)

	ctx := context.Background()

	mc, err := objcache.NewMemoryCache(objcache.MemoryConfig{
		MaxSize:       1024 * 1024, // 1MB — enough for multiple test objects
		MaxObjectSize: 1024,        // 1KB — objects above this are not cached
		TTL:           5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewMemoryCache: %v", err)
	}

	cbStore := store.NewCircuitBreakerStore(testStore, config.CircuitBreakerConfig{
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
		CacheTTL:         60 * time.Second,
	})

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        testBackends,
		Store:           cbStore,
		Order:           testBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		ObjectCache:     mc,
	})

	srv := &server.Server{Manager: mgr}
	srv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{AccessKeyID: "test", SecretAccessKey: "test"},
			},
		},
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpServer := &http.Server{Handler: srv}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Shutdown(ctx) })

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + listener.Addr().String()),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})

	return &cacheTestEnv{client: client, cache: mc}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestCache_HitOnSecondRead verifies that the first full GET populates the
// cache and a subsequent GET for the same key returns identical data from
// the cache (confirmed by checking the cache entry count after each read).
func TestCache_HitOnSecondRead(t *testing.T) {
	env := setupCacheEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "cache-hit")
	body := []byte("cache me please")

	_, err := env.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// First GET — cache miss, populates cache
	resp1, err := env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject #1: %v", err)
	}
	got1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if !bytes.Equal(got1, body) {
		t.Fatalf("body mismatch on first read")
	}

	// Verify cache has the entry
	stats := env.cache.Stats()
	if stats.Entries != 1 {
		t.Errorf("cache entries after first GET = %d, want 1", stats.Entries)
	}

	// Second GET — should be served from cache with identical data
	resp2, err := env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject #2: %v", err)
	}
	got2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !bytes.Equal(got2, body) {
		t.Fatalf("body mismatch on cache hit")
	}
}

// TestCache_InvalidateOnPut verifies that a PutObject invalidates any cached
// entry for that key, so a subsequent GET returns the new data rather than
// stale cached content.
func TestCache_InvalidateOnPut(t *testing.T) {
	env := setupCacheEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "cache-invalidate-put")
	v1 := []byte("version one")
	v2 := []byte("version two — different content")

	// PUT v1
	_, err := env.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(v1),
		ContentLength: aws.Int64(int64(len(v1))),
	})
	if err != nil {
		t.Fatalf("PutObject v1: %v", err)
	}

	// GET to populate cache
	resp, err := env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject v1: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// PUT v2 — should invalidate cache
	_, err = env.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(v2),
		ContentLength: aws.Int64(int64(len(v2))),
	})
	if err != nil {
		t.Fatalf("PutObject v2: %v", err)
	}

	// GET should return v2, not cached v1
	resp, err = env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject v2: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, v2) {
		t.Errorf("expected v2 after PUT invalidation, got %d bytes (want %d)", len(got), len(v2))
	}
}

// TestCache_InvalidateOnDelete verifies that a DeleteObject removes the
// cached entry, so the cache entry count drops to zero and a subsequent
// GET returns a 404 rather than stale cached data.
func TestCache_InvalidateOnDelete(t *testing.T) {
	env := setupCacheEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "cache-invalidate-del")
	body := []byte("delete me after caching")

	_, err := env.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// GET to populate cache
	resp, err := env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if env.cache.Stats().Entries != 1 {
		t.Fatal("expected 1 cache entry before delete")
	}

	// DELETE — should invalidate cache
	_, err = env.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if env.cache.Stats().Entries != 0 {
		t.Error("expected 0 cache entries after delete")
	}

	// GET should now return 404
	_, err = env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
}

// TestCache_OversizedObjectNotCached verifies that objects exceeding the
// configured max_object_size are served correctly but not stored in the
// cache, keeping the cache entry count at zero.
func TestCache_OversizedObjectNotCached(t *testing.T) {
	env := setupCacheEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "cache-oversize")
	// MaxObjectSize is 1KB, create an object well above that threshold
	body := bytes.Repeat([]byte("X"), 1500)

	_, err := env.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// GET — should work but not populate cache
	resp, err := env.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(got) != len(body) {
		t.Errorf("body size = %d, want %d", len(got), len(body))
	}

	if env.cache.Stats().Entries != 0 {
		t.Error("oversized object should not be cached")
	}
}

// TestCache_MultipleObjects verifies that multiple distinct objects are
// cached independently and that re-reading all of them returns the correct
// data for each key from the cache.
func TestCache_MultipleObjects(t *testing.T) {
	env := setupCacheEnv(t)
	ctx := context.Background()

	objects := make(map[string][]byte)
	for i := range 5 {
		key := uniqueKey(t, "cache-multi")
		body := bytes.Repeat([]byte{byte('A' + i)}, 100+i*50)
		objects[key] = body

		_, err := env.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject[%d]: %v", i, err)
		}
	}

	// GET all objects to populate cache
	for key := range objects {
		resp, err := env.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject(%s): %v", key, err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	if env.cache.Stats().Entries != 5 {
		t.Errorf("cache entries = %d, want 5", env.cache.Stats().Entries)
	}

	// Re-read all — should all be cache hits with correct data
	for key, expected := range objects {
		resp, err := env.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject(%s) on re-read: %v", key, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, expected) {
			t.Errorf("body mismatch for %s on cache hit", key)
		}
	}
}
