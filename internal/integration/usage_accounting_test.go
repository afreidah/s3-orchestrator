// -------------------------------------------------------------------------------
// Integration Tests - Backend Usage Accounting
//
// Author: Alex Freidah
//
// Every operation that touches a backend charges it: one API call, plus egress
// for bytes read out of it or ingress for bytes written into it. Those counters
// are what the configured monthly limits are judged against, so an operation
// that charges the wrong backend, the wrong dimension, or the wrong number
// silently moves where the fleet is allowed to do work.
//
// Until now they were only ever exercised against the counter backend directly,
// never through a real operation, which is how enforcement came to be missing
// from whole subsystems while the counters still looked truthful. These tests
// drive real requests against real backends and assert on what was charged.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// usageSnapshot is what a backend had been charged at one moment.
type usageSnapshot struct {
	APICalls int64
	Egress   int64
	Ingress  int64
}

// readUsage reads the live per-backend counters. Read rather than flushed:
// the flush drains into the database on its own schedule, and these tests are
// asserting on what an operation charged, not on when it was persisted.
func readUsage(rt *infra.BackendRuntime, backendName string) usageSnapshot {
	be := rt.Usage().Backend()
	return usageSnapshot{
		APICalls: be.Load(backendName, counter.FieldAPIRequests),
		Egress:   be.Load(backendName, counter.FieldEgressBytes),
		Ingress:  be.Load(backendName, counter.FieldIngressBytes),
	}
}

// sub returns what was charged between two snapshots.
func (u usageSnapshot) sub(prev usageSnapshot) usageSnapshot {
	return usageSnapshot{
		APICalls: u.APICalls - prev.APICalls,
		Egress:   u.Egress - prev.Egress,
		Ingress:  u.Ingress - prev.Ingress,
	}
}

// usageDelta captures the counters for one backend, runs fn, and reports what
// the operation charged.
func usageDelta(rt *infra.BackendRuntime, backendName string, fn func()) usageSnapshot {
	before := readUsage(rt, backendName)
	fn()
	return readUsage(rt, backendName).sub(before)
}

// fleetUsageDelta is usageDelta across several backends at once, for the
// operations that charge two of them: a copy spends egress on its source and
// ingress on its destination, and charging both to one backend would look
// correct in a single-backend assertion.
func fleetUsageDelta(rt *infra.BackendRuntime, names []string, fn func()) map[string]usageSnapshot {
	before := make(map[string]usageSnapshot, len(names))
	for _, name := range names {
		before[name] = readUsage(rt, name)
	}
	fn()
	out := make(map[string]usageSnapshot, len(names))
	for _, name := range names {
		out[name] = readUsage(rt, name).sub(before[name])
	}
	return out
}

// assertCharged fails unless the delta matches want exactly. Exact rather than
// a lower bound: an operation charging more than it should is the failure mode
// that matters, since it spends a budget the fleet is held to.
func assertCharged(t *testing.T, what string, got, want usageSnapshot) {
	t.Helper()
	if got.APICalls != want.APICalls {
		t.Errorf("%s: API calls = %d, want %d", what, got.APICalls, want.APICalls)
	}
	if got.Egress != want.Egress {
		t.Errorf("%s: egress = %d, want %d", what, got.Egress, want.Egress)
	}
	if got.Ingress != want.Ingress {
		t.Errorf("%s: ingress = %d, want %d", what, got.Ingress, want.Ingress)
	}
}

// assertNothingCharged fails unless the backend was left completely untouched,
// which is what a refused operation owes: the point of declining is not to
// spend the budget that was already exhausted.
func assertNothingCharged(t *testing.T, what string, got usageSnapshot) {
	t.Helper()
	if got != (usageSnapshot{}) {
		t.Errorf("%s: charged %+v, want nothing; a refused operation must not touch the backend", what, got)
	}
}

// -------------------------------------------------------------------------
// ACCOUNTING PER OPERATION
// -------------------------------------------------------------------------

// TestUsage_PutChargesIngressOnTargetOnly asserts a write charges ingress to
// the backend that took the bytes and nothing to the others. A write charged
// to the wrong backend moves where the fleet is allowed to keep writing.
func TestUsage_PutChargesIngressOnTargetOnly(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-put")
	body := bytes.Repeat([]byte("P"), 512)

	deltas := fleetUsageDelta(testStack.Runtime, allBackendOrder, func() {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
	})

	target := queryObjectBackend(t, key)
	assertCharged(t, "target "+target, deltas[target], usageSnapshot{APICalls: 1, Ingress: int64(len(body))})
	for _, name := range allBackendOrder {
		if name != target {
			assertNothingCharged(t, "non-target "+name, deltas[name])
		}
	}
}

// TestUsage_GetChargesEgressOnServingBackend asserts a read charges egress to
// whichever backend served it, at the size that came back.
func TestUsage_GetChargesEgressOnServingBackend(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-get")
	body := bytes.Repeat([]byte("G"), 300)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	target := queryObjectBackend(t, key)

	delta := usageDelta(testStack.Runtime, target, func() {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		defer out.Body.Close()
		if _, err := io.Copy(io.Discard, out.Body); err != nil {
			t.Fatalf("drain body: %v", err)
		}
	})

	assertCharged(t, "GET on "+target, delta, usageSnapshot{APICalls: 1, Egress: int64(len(body))})
}

// TestUsage_HeadChargesApiCallWithoutBytes asserts a metadata read costs a
// request and no bandwidth. Charging bytes for a HEAD would let a listing-heavy
// workload exhaust a byte budget it never actually spent.
func TestUsage_HeadChargesApiCallWithoutBytes(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-head")
	body := bytes.Repeat([]byte("H"), 128)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	target := queryObjectBackend(t, key)

	delta := usageDelta(testStack.Runtime, target, func() {
		if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		}); err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
	})

	if delta.Egress != 0 || delta.Ingress != 0 {
		t.Errorf("HEAD charged bytes %+v, want none: it transfers no object data", delta)
	}
}

// TestUsage_DeleteChargesApiCallAndReleasesQuota covers both counters at once:
// a delete spends a request and no bandwidth, and the storage it frees comes
// back off bytes_used. The two are separate ledgers and a delete moves both.
func TestUsage_DeleteChargesApiCallAndReleasesQuota(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-delete")
	body := bytes.Repeat([]byte("D"), 256)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	target := queryObjectBackend(t, key)
	quotaBefore := queryQuotaUsed(t, target)

	delta := usageDelta(testStack.Runtime, target, func() {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		}); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
	})

	if delta.Egress != 0 || delta.Ingress != 0 {
		t.Errorf("DELETE charged bytes %+v, want none", delta)
	}
	if got := queryQuotaUsed(t, target); got != quotaBefore-int64(len(body)) {
		t.Errorf("bytes_used = %d, want %d: the freed storage did not come back off the quota",
			got, quotaBefore-int64(len(body)))
	}
}

// TestUsage_MultipartCompleteChargesPartReadEgress asserts assembly is charged
// for reading the parts back. Completing an upload downloads every part off the
// backend and writes one object in their place, so the operation spends egress
// equal to the upload and ingress equal to the assembled result. Counting only
// the API calls makes a multipart-heavy workload look like it reads nothing.
func TestUsage_MultipartCompleteChargesPartReadEgress(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-mpu-complete")
	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)
	target := queryMultipartBackend(t, uploadID)

	parts := [][]byte{bytes.Repeat([]byte("1"), 300), bytes.Repeat([]byte("2"), 200)}
	completed := make([]types.CompletedPart, len(parts))
	for i, body := range parts {
		number := int32(i + 1)
		up, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(number),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", number, err)
		}
		completed[i] = types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(number)}
	}

	// Measured before completing, which deletes the part objects.
	var partBytes int64
	for i := range parts {
		partBytes += backendRawObjectSize(t, target, multipartPartStoredKey(uploadID, i+1))
	}

	delta := usageDelta(testStack.Runtime, target, func() {
		if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:          aws.String(virtualBucket),
			Key:             aws.String(key),
			UploadId:        aws.String(uploadID),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
		}); err != nil {
			t.Fatalf("CompleteMultipartUpload: %v", err)
		}
	})

	if delta.Egress != partBytes {
		t.Errorf("egress = %d, want %d: assembly reads every part back off the backend",
			delta.Egress, partBytes)
	}
	if assembled := backendObjectSize(t, target, key); delta.Ingress != assembled {
		t.Errorf("ingress = %d, want the %d bytes assembly wrote", delta.Ingress, assembled)
	}
}

// -------------------------------------------------------------------------
// ENFORCEMENT
// -------------------------------------------------------------------------

// exhaustBackends puts every named backend past the given limits for the rest
// of the test, and restores an unmetered fleet afterwards.
//
// The limits and baselines live on the shared manager, so leaving either in
// place would silently refuse work in whatever test ran next.
func exhaustBackends(t *testing.T, names []string, limits core.UsageLimits, spent core.UsageStat) {
	t.Helper()
	byName := make(map[string]core.UsageLimits, len(names))
	for _, name := range names {
		byName[name] = limits
		testStack.Runtime.Usage().SetBaseline(name, spent)
	}
	testStack.Runtime.Usage().UpdateLimits(byName)

	t.Cleanup(func() {
		testStack.Runtime.Usage().UpdateLimits(map[string]core.UsageLimits{})
		testStack.Runtime.Usage().ResetBaselines(allBackendOrder)
		testStack.Objects.LocationCache().Clear()
	})
}

// TestUsage_PutRefusedWhenFleetOutOfIngress asserts a write is turned away
// when no backend has ingress headroom, rather than landing somewhere and
// being counted afterwards. The client sees a storage error; nothing is
// written.
func TestUsage_PutRefusedWhenFleetOutOfIngress(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	exhaustBackends(t, allBackendOrder,
		core.UsageLimits{IngressByteLimit: 100},
		core.UsageStat{IngressBytes: 100})

	key := uniqueKey(t, "usage-put-refused")
	body := bytes.Repeat([]byte("X"), 64)

	deltas := fleetUsageDelta(testStack.Runtime, allBackendOrder, func() {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err == nil {
			t.Fatal("PutObject succeeded with every backend out of ingress budget")
		}
	})

	for _, name := range allBackendOrder {
		assertNothingCharged(t, "refused PUT on "+name, deltas[name])
	}
	if n := queryObjectCopies(t, key); n != 0 {
		t.Errorf("object has %d copies after a refused write, want 0", n)
	}
}

// TestUsage_DeleteAllowedWithEveryBudgetExhausted pins the one operation that
// is deliberately never refused. A delete reduces what a backend holds, so
// gating it on a limit would leave an operator unable to get back under one,
// and a DELETE that returns without removing the object is simply wrong.
func TestUsage_DeleteAllowedWithEveryBudgetExhausted(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "usage-delete-exhausted")
	body := bytes.Repeat([]byte("D"), 128)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Exhausted only after the object exists, so the write above is unaffected.
	exhaustBackends(t, allBackendOrder,
		core.UsageLimits{APIRequestLimit: 1, EgressByteLimit: 1, IngressByteLimit: 1},
		core.UsageStat{APIRequests: 1000, EgressBytes: 1000, IngressBytes: 1000})

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("DeleteObject refused with budgets exhausted: %v", err)
	}
	if n := queryObjectCopies(t, key); n != 0 {
		t.Errorf("object still has %d copies after delete, want 0", n)
	}
}
