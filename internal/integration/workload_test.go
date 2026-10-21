// -------------------------------------------------------------------------------
// Integration Tests - Usage Workload Attribution
//
// Author: Alex Freidah
//
// Drives one isolated orchestrator through the S3 API and every worker entry
// point that reaches a backend, then reads its usage counters directly. Every
// charge has to name the workload that made it: one recorded as unknown means
// some path into the orchestrator does not set the workload on its context.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
)

// TestWorkload_EveryChargeNamesItsWorkload fails when any path charges a
// backend without a workload on its context.
func TestWorkload_EveryChargeNamesItsWorkload(t *testing.T) {
	h := newHarness(t, harnessSpec{Backends: []harnessBackend{
		{Name: "w-minio-1", Quota: 64 << 20},
		{Name: "w-minio-2", Quota: 64 << 20},
		{Name: "w-minio-3", Quota: 64 << 20},
	}})
	ctx := context.Background()

	driveClient(t, h)
	driveWorkers(t, h)

	charged := map[string]bool{}
	for _, b := range h.stack.Runtime.Usage().Backend().Take(ctx) {
		for workload, u := range b.Workloads {
			if !u.IsZero() {
				charged[workload] = true
			}
		}
	}
	if charged[string(audit.WorkloadUnknown)] {
		t.Errorf("a charge was recorded as %q; workloads charged: %v", audit.WorkloadUnknown, charged)
	}
	for _, w := range []audit.Workload{
		audit.WorkloadClient, audit.WorkloadReplication, audit.WorkloadOverReplication,
		audit.WorkloadScrub, audit.WorkloadBackfill, audit.WorkloadReconcile, audit.WorkloadDrain, audit.WorkloadMultipartCleanup,
	} {
		if !charged[string(w)] {
			t.Errorf("no charge recorded for %q; workloads charged: %v", w, charged)
		}
	}
}

// driveClient makes the S3 calls that reach a backend: a put, get, head and
// delete, a completed multipart upload, and one left open for the stale sweep.
func driveClient(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	body := bytes.Repeat([]byte("w"), 4096)
	for _, key := range []string{"wl/a", "wl/b", "wl/c"} {
		h.put(key, body)
	}
	h.get("wl/a")
	if _, err := h.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(virtualBucket), Key: aws.String("wl/b")}); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if _, err := h.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(virtualBucket), Key: aws.String("wl/c")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	completeMultipart(t, h, "wl/mp", body)
	if _, err := h.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket), Key: aws.String("wl/abandoned"),
	}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
}

// completeMultipart uploads body as a one-part multipart upload under key.
func completeMultipart(t *testing.T, h *harness, key string, body []byte) {
	t.Helper()
	ctx := context.Background()
	created, err := h.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload(%s): %v", key, err)
	}
	part, err := h.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key), UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("UploadPart(%s): %v", key, err)
	}
	if _, err := h.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload(%s): %v", key, err)
	}
}

// driveWorkers runs each worker entry point that reaches a backend once, the
// way its tick or an admin request would.
func driveWorkers(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	h.stack.Multipart.CleanupStaleMultipartUploads(ctx, 0)
	h.replicate(3)
	// The harness writes without hashing, and a scrub only reads hashed copies,
	// so the backfill comes first. ScrubKey rather than the tick's Scrub: the
	// tick passes over copies younger than the scrubber's minimum age.
	locs, err := h.store.GetAllObjectLocations(ctx, internalKey("wl/a"))
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	h.workers.Scrubber.HashCopies(ctx, locs, nil)
	if _, err := h.workers.Scrubber.ScrubKey(ctx, internalKey("wl/a")); err != nil {
		t.Fatalf("ScrubKey: %v", err)
	}
	if _, err := h.workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor: 2, BatchSize: 50, Concurrency: 1,
	}, nil); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	h.workers.PendingReaper.ProcessPendingQueue(ctx)
	h.workers.CleanupWorker.ProcessCleanupQueue(ctx)

	rec := reconcile.NewManager(&reconcile.Deps{
		Backends: h.stack.Runtime, Stores: h.store, Usage: h.stack.Runtime.Acct(), Quota: h.stack.Runtime.Quota(),
	})
	for _, name := range h.order {
		if _, err := rec.ReconcileBackend(ctx, name, []string{virtualBucket}); err != nil {
			t.Fatalf("ReconcileBackend(%s): %v", name, err)
		}
	}

	source := h.objectBackend("wl/a")
	if err := h.stack.States.StartDrain(ctx, source); err != nil {
		t.Fatalf("StartDrain(%s): %v", source, err)
	}
	h.drainToCompletion(source)
	if slices.Contains(h.objectBackends("wl/a"), source) {
		t.Fatalf("drain left wl/a on %s", source)
	}
}
