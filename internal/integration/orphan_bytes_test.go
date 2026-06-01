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
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
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

	setOrphanBytes(t, "minio-1", 24)
	setOrphanBytes(t, "minio-2", 48)
	testManager.ClearCache()

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

// TestOrphanBytes_EnqueueIncrementsOrphanBytes is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_EnqueueIncrementsOrphanBytes(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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
}

// TestOrphanBytes_CleanupSuccessDecrementsOrphanBytes is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_CleanupSuccessDecrementsOrphanBytes(t *testing.T) {
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

	processed, failed := testWorkers.CleanupWorker.ProcessCleanupQueue(ctx)
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

	err := testStore.EnqueueCleanup(ctx, "minio-1", "test-bucket/dlq-graduate", "delete_failed", 4096)
	if err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}
	if err := testStore.IncrementOrphanBytes(ctx, "minio-1", 4096); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	// Look up the queue row id and stamp a pre-existing last_error
	// so the move preserves it when an empty argument is passed
	// (covers the "use the row's stored error" branch).
	var id int64
	if err := testDB.QueryRow(
		"SELECT id FROM cleanup_queue WHERE object_key = $1",
		"test-bucket/dlq-graduate",
	).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}
	if _, err := testDB.Exec(
		"UPDATE cleanup_queue SET last_error = $1 WHERE id = $2",
		"transient-earlier", id,
	); err != nil {
		t.Fatalf("set last_error: %v", err)
	}

	moved, err := testStore.MoveCleanupToDLQ(ctx, id, "permanent failure")
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
		t.Errorf("orphan_bytes after move = %d, want 4096 (unchanged)", got)
	}

	depth, err := testStore.CleanupDLQDepth(ctx)
	if err != nil {
		t.Fatalf("CleanupDLQDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("CleanupDLQDepth = %d, want 1", depth)
	}
}

// TestOrphanBytes_MoveCleanupToDLQ_MissingRowIsNoOp is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_MoveCleanupToDLQ_MissingRowIsNoOp(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)
	moved, err := testStore.MoveCleanupToDLQ(ctx, 999999, "irrelevant")
	if err != nil {
		t.Fatalf("MoveCleanupToDLQ: %v", err)
	}
	if moved {
		t.Errorf("expected moved=false when id does not exist (concurrent finaliser race)")
	}
}

// TestOrphanBytes_CleanupZeroSizeSkipsOrphanDecrement is one of the sub-cases extracted from the
// original mega-TestOrphanBytes; behaviour is preserved.
func TestOrphanBytes_CleanupZeroSizeSkipsOrphanDecrement(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

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

	err = testStore.EnqueueCleanup(ctx, backend, internalK, "test_zero", 0)
	if err != nil {
		t.Fatalf("EnqueueCleanup: %v", err)
	}

	setOrphanBytes(t, backend, 200)

	processed, _ := testWorkers.CleanupWorker.ProcessCleanupQueue(ctx)
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}

	orphan := queryOrphanBytes(t, backend)
	if orphan != 200 {
		t.Errorf("orphan_bytes = %d, want 200 (unchanged)", orphan)
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

	setOrphanBytes(t, "minio-2", 148)
	testManager.ClearCache()

	replCfg := config.ReplicationConfig{
		Factor:    2,
		BatchSize: 10,
	}
	created, err := testWorkers.Replicator.Replicate(ctx, replCfg)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if created != 0 {
		t.Errorf("expected 0 replicas created (minio-2 full with orphan_bytes), got %d", created)
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
	created, err := testWorkers.Replicator.Replicate(ctx, replCfg)
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
	spreadManager := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: testBackends,
			Order:    testBackendOrder,
		},
		Stores: proxy.StoreDeps{
			Metadata:  stores,
			Dashboard: testStore,
		},
		Policies: proxy.PolicyConfig{
			CacheTTL:        60 * time.Second,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingSpread,
		},
		Operations: proxy.OperationalDeps{
			Metrics: newMetricsAdapter(testStore),
		},
	})
	_ = spreadManager
	_ = proxytest.BuildWorkers(spreadManager, stores)
	spreadSrv := &s3api.Server{
		Manager: spreadManager,
	}
	_ = spreadSrv
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
}
