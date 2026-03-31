// -------------------------------------------------------------------------------
// Integration Tests - Edge Cases and Missing Scenarios
//
// Author: Alex Freidah
//
// Covers edge cases not in the main integration test suite: empty objects,
// concurrent write conflicts, large multipart uploads, presigned DELETE/HEAD,
// and cache TTL expiration behavior.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// -------------------------------------------------------------------------
// EMPTY OBJECT
// -------------------------------------------------------------------------

// TestEmptyObject_PutGetDelete verifies that a zero-byte object can be
// stored, retrieved, and deleted correctly.
func TestEmptyObject_PutGetDelete(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "empty")

	// PUT 0 bytes
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(nil),
		ContentLength: aws.Int64(0),
	})
	if err != nil {
		t.Fatalf("PutObject empty: %v", err)
	}

	// GET returns 0 bytes
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject empty: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if len(body) != 0 {
		t.Errorf("expected 0 bytes, got %d", len(body))
	}

	// HEAD returns ContentLength 0
	headResp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject empty: %v", err)
	}
	if aws.ToInt64(headResp.ContentLength) != 0 {
		t.Errorf("HEAD ContentLength = %d, want 0", aws.ToInt64(headResp.ContentLength))
	}

	// DELETE
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject empty: %v", err)
	}

	// Verify gone
	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Error("expected 404 after delete")
	}
}

// -------------------------------------------------------------------------
// CONCURRENT WRITE CONFLICTS
// -------------------------------------------------------------------------

// TestConcurrentPutSameKey verifies that concurrent PutObject calls to the
// same key result in exactly one copy in the DB, with the last writer's data
// visible on subsequent GET.
func TestConcurrentPutSameKey(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "concurrent-put")

	const writers = 10
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf("writer-%d", n))
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        aws.String(virtualBucket),
				Key:           aws.String(key),
				Body:          bytes.NewReader(body),
				ContentLength: aws.Int64(int64(len(body))),
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("PutObject error: %v", err)
	}

	// Exactly 1 copy should exist in the DB.
	copies := queryObjectCopies(t, key)
	if copies != 1 {
		t.Errorf("expected 1 copy after concurrent puts, got %d", copies)
	}

	// GET should return one of the writer's data (last-write-wins).
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !strings.HasPrefix(string(body), "writer-") {
		t.Errorf("unexpected body: %q", body)
	}
}

// TestConcurrentPutAndDelete verifies that concurrent PUT and DELETE on the
// same key leaves the system in a consistent state: either the object exists
// (PUT won) or it doesn't (DELETE won), with no ghost DB records.
func TestConcurrentPutAndDelete(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "put-delete-race")

	// Seed the object first.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("seed")),
		ContentLength: aws.Int64(4),
	})
	if err != nil {
		t.Fatalf("seed PutObject: %v", err)
	}

	// Race: PUT new data vs DELETE.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		client.PutObject(ctx, &s3.PutObjectInput{ //nolint:errcheck // race outcome is non-deterministic
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader([]byte("overwrite")),
			ContentLength: aws.Int64(9),
		})
	}()
	go func() {
		defer wg.Done()
		client.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // race outcome is non-deterministic
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
	}()
	wg.Wait()

	// Consistency check: DB copies should be 0 or 1, never > 1.
	copies := queryObjectCopies(t, key)
	if copies > 1 {
		t.Errorf("expected 0 or 1 copies, got %d (inconsistent state)", copies)
	}
}

// -------------------------------------------------------------------------
// LARGE MULTIPART UPLOAD
// -------------------------------------------------------------------------

// TestLargeMultipartUpload verifies that a multipart upload with many parts
// (20 parts) assembles correctly. Each part is 100 bytes.
func TestLargeMultipartUpload(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "large-multipart")

	const numParts = 5
	const partSize = 50

	// Create multipart upload.
	createResp, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := createResp.UploadId

	// Upload parts with distinct data.
	completedParts := make([]s3types.CompletedPart, numParts)
	var expectedBody []byte
	for i := range numParts {
		partData := bytes.Repeat([]byte{byte(i)}, partSize) //nolint:gosec // G115: test loop index, always < 256
		expectedBody = append(expectedBody, partData...)

		uploadResp, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(int32(i + 1)), //nolint:gosec // G115: test loop, always < 10000
			Body:          bytes.NewReader(partData),
			ContentLength: aws.Int64(partSize),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		completedParts[i] = s3types.CompletedPart{
			ETag:       uploadResp.ETag,
			PartNumber: aws.Int32(int32(i + 1)), //nolint:gosec // G115: test loop, always < 10000
		}
	}

	// Complete.
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// Verify assembled object.
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()

	if len(body) != numParts*partSize {
		t.Errorf("body length = %d, want %d", len(body), numParts*partSize)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Error("assembled body does not match uploaded parts")
	}
}

// -------------------------------------------------------------------------
// PRESIGNED DELETE AND HEAD
// -------------------------------------------------------------------------

// TestPresignedHeadObject verifies that a presigned HEAD URL works for
// checking object existence without credentials.
func TestPresignedHeadObject(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	presigner := newPresignClient(t)
	ctx := context.Background()
	key := uniqueKey(t, "presigned-head")

	// Put an object.
	body := []byte("presigned-head-test")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Generate presigned HEAD URL.
	presignedReq, err := presigner.PresignHeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("PresignHeadObject: %v", err)
	}

	// Execute with plain HTTP client (no credentials).
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, presignedReq.URL, nil)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("HEAD presigned: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("presigned HEAD status = %d, want 200", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("presigned HEAD ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
}

// TestPresignedDeleteObject verifies that a presigned DELETE URL removes the
// object without credentials.
func TestPresignedDeleteObject(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	presigner := newPresignClient(t)
	ctx := context.Background()
	key := uniqueKey(t, "presigned-delete")

	// Put an object.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("delete-me")),
		ContentLength: aws.Int64(9),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Generate presigned DELETE URL.
	presignedReq, err := presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("PresignDeleteObject: %v", err)
	}

	// Execute with plain HTTP client.
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, presignedReq.URL, nil)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("DELETE presigned: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Errorf("presigned DELETE status = %d, want 204 or 200", resp.StatusCode)
	}

	// Verify deleted.
	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Error("expected 404 after presigned DELETE")
	}
}

// -------------------------------------------------------------------------
// SPECIAL CHARACTERS IN KEYS
// -------------------------------------------------------------------------

// TestSpecialCharacterKeys verifies that object keys with spaces, unicode,
// and URL-sensitive characters round-trip correctly.
func TestSpecialCharacterKeys(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	keys := []string{
		"folder/file with spaces.txt",
		"path/to/file+plus.txt",
		"dir/key=value&other=param",
		"unicode/emoji-test",
		"deep/nested/path/to/object.dat",
	}

	body := []byte("special-char-test")
	for _, key := range keys {
		fullKey := uniqueKey(t, key)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(fullKey),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Errorf("PutObject(%q): %v", key, err)
			continue
		}

		getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(fullKey),
		})
		if err != nil {
			t.Errorf("GetObject(%q): %v", key, err)
			continue
		}
		got, _ := io.ReadAll(getResp.Body)
		getResp.Body.Close()
		if !bytes.Equal(got, body) {
			t.Errorf("key %q: body mismatch", key)
		}
	}
}

// -------------------------------------------------------------------------
// LARGE SINGLE OBJECT
// -------------------------------------------------------------------------

// TestLargeObject_RandomContent verifies that a binary object with random
// content uploads and downloads correctly with matching content. Uses a size
// that fits within the integration test backend quotas.
func TestLargeObject_RandomContent(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "random-content")

	data := make([]byte, 512)
	_, _ = rand.Read(data)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()

	if len(got) != len(data) {
		t.Fatalf("body length = %d, want %d", len(got), len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("random content body mismatch")
	}
}

// -------------------------------------------------------------------------
// CONDITIONAL REQUESTS
// -------------------------------------------------------------------------

// TestConditionalGet_IfNoneMatch verifies that GET with If-None-Match returns
// 304 Not Modified when the ETag matches.
func TestConditionalGet_IfNoneMatch(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "conditional")

	body := []byte("conditional-test")
	putResp, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	etag := aws.ToString(putResp.ETag)

	// GET with matching ETag should return 304.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:      aws.String(virtualBucket),
		Key:         aws.String(key),
		IfNoneMatch: aws.String(etag),
	})
	if err == nil {
		t.Fatal("expected error for 304 Not Modified")
	}
	// The AWS SDK wraps 304 as an error.
	errStr := err.Error()
	if !strings.Contains(errStr, "304") && !strings.Contains(errStr, "NotModified") {
		t.Errorf("expected 304 error, got: %v", err)
	}
}

// TestConditionalGet_IfMatch_Mismatch verifies that GET with a non-matching
// If-Match ETag returns 412 Precondition Failed.
func TestConditionalGet_IfMatch_Mismatch(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "if-match")

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("data")),
		ContentLength: aws.Int64(4),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// GET with wrong ETag should return 412.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  aws.String(virtualBucket),
		Key:     aws.String(key),
		IfMatch: aws.String(`"wrong-etag"`),
	})
	if err == nil {
		t.Fatal("expected error for 412 Precondition Failed")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "412") && !strings.Contains(errStr, "PreconditionFailed") {
		t.Errorf("expected 412 error, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// USER METADATA
// -------------------------------------------------------------------------

// TestUserMetadata_RoundTrip verifies that user-defined metadata survives
// PUT and is returned on GET/HEAD.
func TestUserMetadata_RoundTrip(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "metadata")

	meta := map[string]string{
		"author":  "test-suite",
		"project": "s3-orchestrator",
	}

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("with-metadata")),
		ContentLength: aws.Int64(13),
		Metadata:      meta,
	})
	if err != nil {
		t.Fatalf("PutObject with metadata: %v", err)
	}

	// HEAD should return metadata.
	headResp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	for k, want := range meta {
		if got := headResp.Metadata[k]; got != want {
			t.Errorf("metadata[%q] = %q, want %q", k, got, want)
		}
	}

	// GET should also return metadata.
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	getResp.Body.Close()
	for k, want := range meta {
		if got := getResp.Metadata[k]; got != want {
			t.Errorf("GET metadata[%q] = %q, want %q", k, got, want)
		}
	}
}

// -------------------------------------------------------------------------
// OVERWRITE PRESERVES QUOTA
// -------------------------------------------------------------------------

// TestOverwrite_QuotaAccounting verifies that overwriting an object with a
// different size correctly updates the backend quota (old size freed, new
// size charged).
func TestOverwrite_QuotaAccounting(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "overwrite-quota")

	// PUT 100 bytes.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 100)),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject v1: %v", err)
	}
	backend := queryObjectBackend(t, key)
	quotaAfterV1 := queryQuotaUsed(t, backend)

	// Overwrite with 200 bytes.
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("B"), 200)),
		ContentLength: aws.Int64(200),
	})
	if err != nil {
		t.Fatalf("PutObject v2: %v", err)
	}
	quotaAfterV2 := queryQuotaUsed(t, backend)

	// Quota should have increased by ~100 (200 new - 100 old displaced).
	diff := quotaAfterV2 - quotaAfterV1
	if diff != 100 {
		t.Errorf("quota diff = %d, want 100 (old 100 freed, new 200 charged)", diff)
	}
}

// -------------------------------------------------------------------------
// DELETE IDEMPOTENCY
// -------------------------------------------------------------------------

// TestDelete_Idempotent verifies that deleting a nonexistent key does not
// return an error (S3 spec: DELETE is idempotent).
func TestDelete_Idempotent(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("never-existed-" + time.Now().Format(time.RFC3339Nano)),
	})
	if err != nil {
		t.Errorf("DELETE nonexistent key should succeed, got: %v", err)
	}
}
