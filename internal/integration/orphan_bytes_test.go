// -------------------------------------------------------------------------------
// Integration Tests - Orphan Bytes Tracking
//
// Author: Alex Freidah
//
// End-to-end tests for the orphan_bytes tracking feature: cleanup queue lifecycle,
// capacity blocking, overwrite displaced copy cleanup, and quota stat reporting.
// Runs against real MinIO and PostgreSQL containers.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/server"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// -------------------------------------------------------------------------
// ORPHAN BYTES — CAPACITY BLOCKING
// -------------------------------------------------------------------------

func TestOrphanBytes(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	t.Run("OrphanBytesBlockWrite", func(t *testing.T) {
		resetState(t)

		// minio-1 has 1024 byte quota, minio-2 has 2048 byte quota.
		// Fill minio-1 to 900 bytes.
		fillKey := uniqueKey(t, "orphan-block")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fillKey),
			Body:          bytes.NewReader(bytes.Repeat([]byte("F"), 900)),
			ContentLength: aws.Int64(900),
		})
		if err != nil {
			t.Fatalf("fill PutObject: %v", err)
		}

		// Without orphan bytes, 120 bytes would fit on minio-1 (available = 1024-900 = 124).
		// Set orphan_bytes=10 on minio-1, making available = 1024-900-10 = 114.
		// 120 > 114, so it should overflow to minio-2.
		setOrphanBytes(t, "minio-1", 10)
		testManager.ClearCache()

		overflowKey := uniqueKey(t, "orphan-block")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(overflowKey),
			Body:          bytes.NewReader(bytes.Repeat([]byte("O"), 120)),
			ContentLength: aws.Int64(120),
		})
		if err != nil {
			t.Fatalf("overflow PutObject: %v", err)
		}

		backend := queryObjectBackend(t, overflowKey)
		if backend != "minio-2" {
			t.Errorf("expected overflow to minio-2 due to orphan_bytes, got %q", backend)
		}
	})

	t.Run("OrphanBytesBlockAllBackends507", func(t *testing.T) {
		resetState(t)

		// Fill minio-1 to 1000 bytes, minio-2 to 2000 bytes.
		fill1Key := uniqueKey(t, "orphan-507")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fill1Key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 1000)),
			ContentLength: aws.Int64(1000),
		})
		if err != nil {
			t.Fatalf("fill minio-1: %v", err)
		}

		fill2Key := uniqueKey(t, "orphan-507")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fill2Key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("B"), 2000)),
			ContentLength: aws.Int64(2000),
		})
		if err != nil {
			t.Fatalf("fill minio-2: %v", err)
		}

		// Set orphan bytes to consume remaining capacity on both backends.
		// minio-1: available = 1024 - 1000 - 24 = 0
		// minio-2: available = 2048 - 2000 - 48 = 0
		setOrphanBytes(t, "minio-1", 24)
		setOrphanBytes(t, "minio-2", 48)
		testManager.ClearCache()

		// Even a 1-byte write should be rejected.
		tinyKey := uniqueKey(t, "orphan-507")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(tinyKey),
			Body:          bytes.NewReader([]byte("X")),
			ContentLength: aws.Int64(1),
		})
		if err == nil {
			t.Fatal("expected write rejection (507) when orphan_bytes fill capacity, got success")
		}
	})

	// -------------------------------------------------------------------------
	// ORPHAN BYTES — CLEANUP QUEUE LIFECYCLE
	// -------------------------------------------------------------------------

	t.Run("EnqueueIncrementsOrphanBytes", func(t *testing.T) {
		resetState(t)

		// Directly enqueue a cleanup item via the store and verify orphan_bytes.
		err := testStore.EnqueueCleanup(ctx, "minio-1", "test-bucket/orphan-test-key", "test_reason", 512)
		if err != nil {
			t.Fatalf("EnqueueCleanup: %v", err)
		}
		err = testStore.IncrementOrphanBytes(ctx, "minio-1", 512)
		if err != nil {
			t.Fatalf("IncrementOrphanBytes: %v", err)
		}

		orphan := queryOrphanBytes(t, "minio-1")
		if orphan != 512 {
			t.Errorf("orphan_bytes = %d, want 512", orphan)
		}

		count := queryCleanupQueueCount(t, "minio-1")
		if count != 1 {
			t.Errorf("cleanup_queue count = %d, want 1", count)
		}

		key, size, _ := queryCleanupQueueItem(t, "minio-1")
		if key != "test-bucket/orphan-test-key" {
			t.Errorf("cleanup_queue object_key = %q, want %q", key, "test-bucket/orphan-test-key")
		}
		if size != 512 {
			t.Errorf("cleanup_queue size_bytes = %d, want 512", size)
		}
	})

	t.Run("CleanupSuccessDecrementsOrphanBytes", func(t *testing.T) {
		resetState(t)

		// Upload an object via the proxy so it exists on a backend.
		key := uniqueKey(t, "cleanup-decr")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("C"), 100)),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}

		backend := queryObjectBackend(t, key)

		// Simulate: enqueue the object for cleanup (as if a delete had failed)
		// and set orphan_bytes. The object still exists on the backend, so the
		// cleanup worker will successfully delete it.
		internalK := internalKey(key)
		err = testStore.EnqueueCleanup(ctx, backend, internalK, "test_reason", 100)
		if err != nil {
			t.Fatalf("EnqueueCleanup: %v", err)
		}
		err = testStore.IncrementOrphanBytes(ctx, backend, 100)
		if err != nil {
			t.Fatalf("IncrementOrphanBytes: %v", err)
		}

		orphanBefore := queryOrphanBytes(t, backend)
		if orphanBefore != 100 {
			t.Fatalf("orphan_bytes before cleanup = %d, want 100", orphanBefore)
		}

		// Run the cleanup worker — it should successfully delete the object
		// and decrement orphan_bytes.
		processed, failed := testManager.CleanupWorker.ProcessCleanupQueue(ctx)
		if processed != 1 {
			t.Errorf("processed = %d, want 1", processed)
		}
		if failed != 0 {
			t.Errorf("failed = %d, want 0", failed)
		}

		orphanAfter := queryOrphanBytes(t, backend)
		if orphanAfter != 0 {
			t.Errorf("orphan_bytes after cleanup = %d, want 0", orphanAfter)
		}

		queueCount := queryCleanupQueueCount(t, backend)
		if queueCount != 0 {
			t.Errorf("cleanup_queue count after cleanup = %d, want 0", queueCount)
		}
	})

	t.Run("CleanupZeroSizeSkipsOrphanDecrement", func(t *testing.T) {
		resetState(t)

		// Upload an object.
		key := uniqueKey(t, "cleanup-zero")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("Z"), 50)),
			ContentLength: aws.Int64(50),
		})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}

		backend := queryObjectBackend(t, key)
		internalK := internalKey(key)

		// Enqueue with size_bytes = 0 (legacy item or unknown-size cleanup).
		err = testStore.EnqueueCleanup(ctx, backend, internalK, "test_zero", 0)
		if err != nil {
			t.Fatalf("EnqueueCleanup: %v", err)
		}

		// Set orphan_bytes to some baseline to verify it doesn't go negative.
		setOrphanBytes(t, backend, 200)

		processed, _ := testManager.CleanupWorker.ProcessCleanupQueue(ctx)
		if processed != 1 {
			t.Errorf("processed = %d, want 1", processed)
		}

		// Orphan bytes should remain at 200 — zero-size items don't decrement.
		orphan := queryOrphanBytes(t, backend)
		if orphan != 200 {
			t.Errorf("orphan_bytes = %d, want 200 (unchanged)", orphan)
		}
	})

	// -------------------------------------------------------------------------
	// ORPHAN BYTES — QUOTA STATS REPORTING
	// -------------------------------------------------------------------------

	t.Run("QuotaStatsIncludeOrphanBytes", func(t *testing.T) {
		resetState(t)

		setOrphanBytes(t, "minio-1", 256)
		setOrphanBytes(t, "minio-2", 512)

		stats, err := testStore.GetQuotaStats(ctx)
		if err != nil {
			t.Fatalf("GetQuotaStats: %v", err)
		}

		s1, ok := stats["minio-1"]
		if !ok {
			t.Fatal("minio-1 not in quota stats")
		}
		if s1.OrphanBytes != 256 {
			t.Errorf("minio-1 OrphanBytes = %d, want 256", s1.OrphanBytes)
		}

		s2, ok := stats["minio-2"]
		if !ok {
			t.Fatal("minio-2 not in quota stats")
		}
		if s2.OrphanBytes != 512 {
			t.Errorf("minio-2 OrphanBytes = %d, want 512", s2.OrphanBytes)
		}
	})

	// -------------------------------------------------------------------------
	// ORPHAN BYTES — REPLICATION TARGET SELECTION
	// -------------------------------------------------------------------------

	t.Run("ReplicationRespectsOrphanBytes", func(t *testing.T) {
		resetState(t)

		// Upload a small object — lands on minio-1 (pack routing).
		key := uniqueKey(t, "repl-orphan")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("R"), 100)),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}

		srcBackend := queryObjectBackend(t, key)
		if srcBackend != "minio-1" {
			t.Fatalf("expected object on minio-1 (pack routing), got %q", srcBackend)
		}

		// Fill minio-2 close to capacity and set orphan_bytes to consume the rest.
		fill2Key := uniqueKey(t, "repl-orphan-fill")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fill2Key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("F"), 1900)),
			ContentLength: aws.Int64(1900),
		})
		if err != nil {
			t.Fatalf("fill minio-2: %v", err)
		}

		// minio-2: available = 2048 - 1900 - 148 = 0. No room for 100-byte replica.
		setOrphanBytes(t, "minio-2", 148)
		testManager.ClearCache()

		replCfg := config.ReplicationConfig{
			Factor:    2,
			BatchSize: 10,
		}
		created, err := testManager.Replicator.Replicate(ctx, replCfg)
		if err != nil {
			t.Fatalf("Replicate: %v", err)
		}

		// Replication should fail to find a target (minio-2 full with orphan_bytes).
		if created != 0 {
			t.Errorf("expected 0 replicas created (minio-2 full with orphan_bytes), got %d", created)
		}

		copies := queryObjectCopies(t, key)
		if copies != 1 {
			t.Errorf("expected 1 copy (no replication target), got %d", copies)
		}
	})

	// -------------------------------------------------------------------------
	// ORPHAN BYTES — OVERWRITE DISPLACED COPIES
	// -------------------------------------------------------------------------

	t.Run("OverwriteDisplacedCopiesCleanedUp", func(t *testing.T) {
		resetState(t)

		// Upload an object — lands on minio-1.
		key := uniqueKey(t, "overwrite-displaced")
		body := bytes.Repeat([]byte("V"), 100)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("PutObject v1: %v", err)
		}

		// Replicate to get 2 copies (one on each backend).
		replCfg := config.ReplicationConfig{
			Factor:    2,
			BatchSize: 10,
		}
		created, err := testManager.Replicator.Replicate(ctx, replCfg)
		if err != nil {
			t.Fatalf("Replicate: %v", err)
		}
		if created != 1 {
			t.Fatalf("expected 1 replica created, got %d", created)
		}

		copies := queryObjectCopies(t, key)
		if copies != 2 {
			t.Fatalf("expected 2 copies after replication, got %d", copies)
		}

		backends := queryObjectBackends(t, key)
		t.Logf("before overwrite: copies on %v", backends)

		// Overwrite the object — the new version lands on one backend,
		// the stale copy on the other backend becomes a displaced copy.
		newBody := bytes.Repeat([]byte("W"), 150)
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(newBody),
			ContentLength: aws.Int64(150),
		})
		if err != nil {
			t.Fatalf("PutObject v2 (overwrite): %v", err)
		}

		// After overwrite, there should be only 1 copy in the DB.
		copiesAfter := queryObjectCopies(t, key)
		if copiesAfter != 1 {
			t.Errorf("expected 1 copy after overwrite, got %d", copiesAfter)
		}

		// The displaced copy's backend delete should have succeeded (the object
		// existed on the backend), so no cleanup queue entry should be created
		// and orphan_bytes should remain at 0.
		for _, be := range testBackendOrder {
			orphan := queryOrphanBytes(t, be)
			if orphan != 0 {
				t.Errorf("%s orphan_bytes = %d after overwrite, want 0 (successful displacement)", be, orphan)
			}
		}

		// Verify the object is readable with the new content.
		resp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject after overwrite: %v", err)
		}
		defer resp.Body.Close()

		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		if !bytes.Equal(buf.Bytes(), newBody) {
			t.Errorf("body mismatch after overwrite: got %d bytes, want %d", buf.Len(), len(newBody))
		}
	})
}

// -------------------------------------------------------------------------
// ORPHAN BYTES — SPREAD ROUTING (separate manager with "spread" strategy)
// -------------------------------------------------------------------------

func TestOrphanBytesSpreadRouting(t *testing.T) {
	ctx := context.Background()

	// Build a spread-strategy manager sharing the same store and backends.
	spreadCBStore := store.NewCircuitBreakerStore(testStore, config.CircuitBreakerConfig{
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
		CacheTTL:         60 * time.Second,
	})
	spreadManager := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        testBackends,
		Store:           spreadCBStore,
		Order:           testBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "spread",
	})

	spreadSrv := &server.Server{
		Manager: spreadManager,
	}
	spreadSrv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{{
		Name: virtualBucket,
		Credentials: []config.CredentialConfig{{
			AccessKeyID:     "test",
			SecretAccessKey: "test",
		}},
	}}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{
		Handler:      spreadSrv,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(ctx)

	spreadClient := s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + listener.Addr().String()),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})

	t.Run("SpreadRoutingRespectsOrphanBytes", func(t *testing.T) {
		resetState(t)

		// Use the pack-routed global client to fill minio-1 deterministically.
		// minio-1: 1024 limit. Put 500 bytes → ratio = (500+0)/1024 = 0.488.
		packClient := newS3Client(t)
		fill1Key := uniqueKey(t, "spread-orphan")
		_, err := packClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fill1Key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 500)),
			ContentLength: aws.Int64(500),
		})
		if err != nil {
			t.Fatalf("fill minio-1: %v", err)
		}
		if be := queryObjectBackend(t, fill1Key); be != "minio-1" {
			t.Fatalf("expected fill on minio-1 (pack routing), got %q", be)
		}

		// Set orphan_bytes on minio-2 to 1500:
		// effective ratio = (0 + 1500) / 2048 = 0.732 > 0.488
		// Spread routing should prefer minio-1 (lower effective ratio).
		setOrphanBytes(t, "minio-2", 1500)
		spreadManager.ClearCache()

		spreadKey := uniqueKey(t, "spread-orphan")
		_, err = spreadClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(spreadKey),
			Body:          bytes.NewReader(bytes.Repeat([]byte("S"), 100)),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("spread PutObject: %v", err)
		}

		backend := queryObjectBackend(t, spreadKey)
		if backend != "minio-1" {
			t.Errorf("spread routing should prefer minio-1 (lower effective ratio), got %q", backend)
		}
	})
}
