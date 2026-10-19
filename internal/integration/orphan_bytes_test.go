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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// -------------------------------------------------------------------------
// ORPHAN BYTES  -  CAPACITY BLOCKING
// -------------------------------------------------------------------------

// TestOrphanBytes_OrphanBytesBlockWrite is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_OrphanBytesBlockWrite(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	seedOrphanBytes(t, "minio-1", 10)
	testStack.Objects.LocationCache().Clear()

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
}

// TestOrphanBytes_OrphanBytesBlockAllBackends507 is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_OrphanBytesBlockAllBackends507(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	seedOrphanBytes(t, "minio-1", 24)
	seedOrphanBytes(t, "minio-2", 48)
	testStack.Objects.LocationCache().Clear()

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
}

// TestOrphanBytes_QueuedRowCountsAsOrphanBytes asserts a queued cleanup row's
// size is what backend_capacity reports as the backend's orphan bytes.
func TestOrphanBytes_QueuedRowCountsAsOrphanBytes(t *testing.T) {
	resetState(t)

	seedCleanupRow(t, "minio-1", "test-bucket/orphan-test-key", "test_reason", 512)

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
}

// TestOrphanBytes_SettledRowStopsCounting asserts a cleanup row the worker
// settles stops counting as orphan bytes once its row is gone.
func TestOrphanBytes_SettledRowStopsCounting(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	seedCleanupRow(t, backend, internalKey(key), "test_reason", 100)

	orphanBefore := queryOrphanBytes(t, backend)
	if orphanBefore != 100 {
		t.Fatalf("orphan_bytes before cleanup = %d, want 100", orphanBefore)
	}

	cleanSum := testWorkers.CleanupWorker.ProcessCleanupQueue(ctx)
	processed, failed := cleanSum.Succeeded, cleanSum.Failed
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
}

// TestOrphanBytes_MoveCleanupToDLQ_GraduatesQueueRow is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_MoveCleanupToDLQ_GraduatesQueueRow(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	// Stamp a pre-existing last_error so the move has one to overwrite.
	id := seedCleanupRow(t, "minio-1", "test-bucket/dlq-graduate", "delete_failed", 4096)
	if _, err := testDB.Exec(
		"UPDATE cleanup_queue SET last_error = $1 WHERE id = $2",
		"transient-earlier", id,
	); err != nil {
		t.Fatalf("set last_error: %v", err)
	}

	item := claimSeededCleanupRow(t, ctx, id)
	moved, err := testStore.MoveCleanupToDLQ(ctx, &item, "permanent failure")
	if err != nil {
		t.Fatalf("MoveCleanupToDLQ: %v", err)
	}
	if !moved {
		t.Errorf("expected moved=true on first call")
	}

	if got := queryCleanupQueueCount(t, "minio-1"); got != 0 {
		t.Errorf("cleanup_queue count after move = %d, want 0", got)
	}

	// cleanup_dlq must hold the row with full forensic context.
	var (
		origID    int64
		backend   string
		key       string
		reason    string
		size      int64
		attempts  int32
		lastError string
	)
	if err := testDB.QueryRow(
		`SELECT original_id, backend_name, object_key, reason, size_bytes, attempts, COALESCE(last_error, '')
				 FROM cleanup_dlq WHERE original_id = $1`, id,
	).Scan(&origID, &backend, &key, &reason, &size, &attempts, &lastError); err != nil {
		t.Fatalf("probe DLQ: %v", err)
	}
	if origID != id || backend != "minio-1" || key != "test-bucket/dlq-graduate" ||
		reason != "delete_failed" || size != 4096 || lastError != "permanent failure" {
		t.Errorf("DLQ row mismatch: orig=%d backend=%q key=%q reason=%q size=%d err=%q",
			origID, backend, key, reason, size, lastError)
	}

	if got := queryOrphanBytes(t, "minio-1"); got != 4096 {
		t.Errorf("orphan_bytes after move = %d, want 4096 (dead-lettered rows still count)", got)
	}

	depth, err := testStore.CleanupDLQDepth(ctx)
	if err != nil {
		t.Fatalf("CleanupDLQDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("CleanupDLQDepth = %d, want 1", depth)
	}
}

// claimSeededCleanupRow claims the seeded row with id the way the cleanup
// worker does and returns it with the claim it was stamped with.
func claimSeededCleanupRow(t *testing.T, ctx context.Context, id int64) core.CleanupItem {
	t.Helper()
	items, err := testStore.ClaimPendingCleanups(ctx, 1000, "orphan-test", time.Now())
	if err != nil {
		t.Fatalf("ClaimPendingCleanups: %v", err)
	}
	for i := range items {
		if items[i].ID == id {
			return items[i]
		}
	}
	t.Fatalf("seeded cleanup row %d was not claimed", id)
	return core.CleanupItem{}
}

// TestOrphanBytes_MoveCleanupToDLQ_MissingRowIsNoOp is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_MoveCleanupToDLQ_MissingRowIsNoOp(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)
	missing := &core.CleanupItem{ID: 999999, Claim: &core.CleanupClaim{At: time.Now(), By: "orphan-test"}}
	moved, err := testStore.MoveCleanupToDLQ(ctx, missing, "irrelevant")
	if err != nil {
		t.Fatalf("MoveCleanupToDLQ: %v", err)
	}
	if moved {
		t.Errorf("expected moved=false when id does not exist (concurrent finaliser race)")
	}
}

// TestOrphanBytes_DeleteSettlesItsCleanupRows asserts a DELETE queues the
// object's bytes with the delete and settles them before answering: the copy
// is gone from the backend and no cleanup row or orphan byte is left.
func TestOrphanBytes_DeleteSettlesItsCleanupRows(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	resetState(t)

	key := uniqueKey(t, "delete-settles")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("D"), 50)),
		ContentLength: aws.Int64(50),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	backend := queryObjectBackend(t, key)
	path := storagePath(t, backend, key)

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if got := queryCleanupQueueCount(t, backend); got != 0 {
		t.Errorf("cleanup_queue rows after delete = %d, want 0 (settled on the request)", got)
	}
	if got := queryOrphanBytes(t, backend); got != 0 {
		t.Errorf("orphan_bytes after delete = %d, want 0", got)
	}
	if _, err := allBackends[backend].HeadObject(ctx, path); err == nil {
		t.Errorf("backend %s still holds %s after the delete settled", backend, path)
	}
}

// TestOrphanBytes_QuotaStatsIncludeOrphanBytes is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_QuotaStatsIncludeOrphanBytes(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	seedOrphanBytes(t, "minio-1", 256)
	seedOrphanBytes(t, "minio-2", 512)

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
}

// TestOrphanBytes_ReplicationRespectsOrphanBytes is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_ReplicationRespectsOrphanBytes(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	seedOrphanBytes(t, "minio-2", 148)
	testStack.Objects.LocationCache().Clear()

	replCfg := config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}
	replSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if replSum.CopiesCreated != 0 {
		t.Errorf("expected 0 replicas created (minio-2 full with orphan_bytes), got %d", replSum.CopiesCreated)
	}

	copies := queryObjectCopies(t, key)
	if copies != 1 {
		t.Errorf("expected 1 copy (no replication target), got %d", copies)
	}
}

// TestOrphanBytes_OverwriteDisplacedCopiesCleanedUp is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_OverwriteDisplacedCopiesCleanedUp(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	replCfg := config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}
	replSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if replSum.CopiesCreated != 1 {
		t.Fatalf("expected 1 replica created, got %d", replSum.CopiesCreated)
	}

	copies := queryObjectCopies(t, key)
	if copies != 2 {
		t.Fatalf("expected 2 copies after replication, got %d", copies)
	}

	backends := queryObjectBackends(t, key)
	t.Logf("before overwrite: copies on %v", backends)

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

	copiesAfter := queryObjectCopies(t, key)
	if copiesAfter != 1 {
		t.Errorf("expected 1 copy after overwrite, got %d", copiesAfter)
	}

	for _, be := range testBackendOrder {
		orphan := queryOrphanBytes(t, be)
		if orphan != 0 {
			t.Errorf("%s orphan_bytes = %d after overwrite, want 0 (successful displacement)", be, orphan)
		}
		if got := queryCleanupQueueCount(t, be); got != 0 {
			t.Errorf("%s cleanup_queue rows = %d after overwrite, want 0 (settled on the request)", be, got)
		}
	}

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
}

// -------------------------------------------------------------------------
// ORPHAN BYTES  -  SPREAD ROUTING (separate manager with "spread" strategy)
// -------------------------------------------------------------------------

// TestOrphanBytesSpreadRouting_SpreadRoutingRespectsOrphanBytes is one of the sub-cases extracted from the
// original mega-TestOrphanBytesSpreadRouting; behaviour is preserved.
func TestOrphanBytesSpreadRouting_SpreadRoutingRespectsOrphanBytes(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	stores := newStores(testStore)
	spreadStack := proxytest.New(t, stores, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        testBackends,
			Order:           testBackendOrder,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingSpread,
			Metrics:         newMetricsAdapter(testStore),
		}),
		CacheTTL:       60 * time.Second,
		BackendTimeout: 30 * time.Second,
	})
	registerStack(t, spreadStack)
	_ = proxytest.BuildWorkers(spreadStack, stores)
	spreadSrv := &s3api.Server{
		Objects:   spreadStack.Objects,
		Multipart: spreadStack.Multipart,
	}
	_ = spreadSrv
	spreadSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{{
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
	_ = httpSrv
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(ctx)
	spreadClient := s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + listener.Addr().String()),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})
	_ = spreadClient

	resetState(t)

	packClient := newS3Client(t)
	fill1Key := uniqueKey(t, "spread-orphan")
	_, err = packClient.PutObject(ctx, &s3.PutObjectInput{
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

	seedOrphanBytes(t, "minio-2", 1500)
	spreadStack.Objects.LocationCache().Clear()

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
}
