// -------------------------------------------------------------------------------
// End-to-End Integration Tests
//
// Author: Alex Freidah
//
// Exercises full S3 lifecycles - single-object PUT/GET/DELETE/COPY,
// multipart upload/list/complete/abort, and the canonical S3 error
// responses - against real MinIO and PostgreSQL containers. Gated behind
// the `integration` build tag and run in CI with testcontainers so the
// public S3 surface stays bug-compatible with real clients.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// -------------------------------------------------------------------------
// E2E LIFECYCLE TESTS
// -------------------------------------------------------------------------

// TestE2E_FullLifecycle drives one object through PUT, HEAD, GET, LIST,
// DELETE, then asserts the post-delete GET returns NoSuchKey. Each
// phase delegates assertion detail to a phase helper so the cognitive
// complexity stays low.
func TestE2E_FullLifecycle(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "e2e")
	body := []byte("end-to-end test payload")

	e2ePut(t, ctx, client, key, body, "text/plain")
	e2eHead(t, ctx, client, key, len(body), "text/plain")
	e2eGetEqual(t, ctx, client, key, body)
	e2eListSingle(t, ctx, client, key, len(body))
	e2eDelete(t, ctx, client, key)
	e2eAssertNotFound(t, ctx, client, key)
}

// e2ePut PUTs body under key and asserts the response carries an ETag.
func e2ePut(t *testing.T, ctx context.Context, client *s3.Client, key string, body []byte, contentType string) {
	t.Helper()
	resp, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if resp.ETag == nil || *resp.ETag == "" {
		t.Error("PutObject should return an ETag")
	}
}

// e2eHead asserts HeadObject reports the expected size and content type.
func e2eHead(t *testing.T, ctx context.Context, client *s3.Client, key string, wantSize int, wantCT string) {
	t.Helper()
	resp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if resp.ContentLength == nil || *resp.ContentLength != int64(wantSize) {
		t.Errorf("HeadObject ContentLength = %v, want %d", resp.ContentLength, wantSize)
	}
	if resp.ContentType == nil || *resp.ContentType != wantCT {
		t.Errorf("HeadObject ContentType = %v, want %s", resp.ContentType, wantCT)
	}
}

// e2eGetEqual asserts GetObject returns the expected body bytes and a
// matching Content-Length header.
func e2eGetEqual(t *testing.T, ctx context.Context, client *s3.Client, key string, want []byte) {
	t.Helper()
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("GetObject body = %q, want %q", got, want)
	}
	if resp.ContentLength == nil || *resp.ContentLength != int64(len(want)) {
		t.Errorf("GetObject ContentLength = %v, want %d", resp.ContentLength, len(want))
	}
}

// e2eListSingle asserts a ListObjectsV2 prefix scan returns exactly the
// supplied key with the expected size.
func e2eListSingle(t *testing.T, ctx context.Context, client *s3.Client, key string, wantSize int) {
	t.Helper()
	resp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(resp.Contents) != 1 {
		t.Fatalf("ListObjectsV2 returned %d objects, want 1", len(resp.Contents))
	}
	entry := resp.Contents[0]
	if entry.Key == nil || *entry.Key != key {
		t.Errorf("ListObjectsV2 key = %v, want %q", entry.Key, key)
	}
	if entry.Size == nil || *entry.Size != int64(wantSize) {
		t.Errorf("ListObjectsV2 size = %v, want %d", entry.Size, wantSize)
	}
}

// e2eDelete removes the object under key and fails the test if the
// underlying delete call errors.
func e2eDelete(t *testing.T, ctx context.Context, client *s3.Client, key string) {
	t.Helper()
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
}

// e2eAssertNotFound issues a GetObject for key and expects a NoSuchKey
// error, confirming the prior delete actually removed metadata.
func e2eAssertNotFound(t *testing.T, ctx context.Context, client *s3.Client, key string) {
	t.Helper()
	_, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("GetObject after delete should fail")
	}
	assertS3ErrorCode(t, err, "NoSuchKey")
}

// TestE2E_MultipartLifecycle verifies the e2 e multipart lifecycle contract.
// Asserts that CreateMultipartUpload:.
func TestE2E_MultipartLifecycle(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "e2e-mp")

	// CreateMultipartUpload
	createResp, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(virtualBucket),
		Key:         aws.String(key),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := *createResp.UploadId

	// UploadPart  -  two 100-byte parts (sized to fit test backend quotas).
	partSize := 100
	part1Data := bytes.Repeat([]byte("A"), partSize)
	part2Data := bytes.Repeat([]byte("B"), partSize)

	upload1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(1),
		Body:          bytes.NewReader(part1Data),
		ContentLength: aws.Int64(int64(partSize)),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}

	upload2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		UploadId:      aws.String(uploadID),
		PartNumber:    aws.Int32(2),
		Body:          bytes.NewReader(part2Data),
		ContentLength: aws.Int64(int64(partSize)),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	// ListParts
	listPartsResp, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(listPartsResp.Parts) != 2 {
		t.Fatalf("ListParts returned %d parts, want 2", len(listPartsResp.Parts))
	}

	// CompleteMultipartUpload
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: upload1.ETag},
				{PartNumber: aws.Int32(2), ETag: upload2.ETag},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// GET the assembled object.
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after complete: %v", err)
	}
	defer getResp.Body.Close()

	got, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	expected := append(part1Data, part2Data...)
	if !bytes.Equal(got, expected) {
		t.Errorf("assembled object size = %d, want %d", len(got), len(expected))
	}
}

// TestE2E_ErrorResponses_GetNonexistent is one of the sub-cases extracted from the
// original mega-TestE2E_ErrorResponses; behaviour is preserved.
func TestE2E_ErrorResponses_GetNonexistent(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	_, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("does-not-exist-ever"),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
	assertS3ErrorCode(t, err, "NoSuchKey")
}

// TestE2E_ErrorResponses_HeadNonexistent is one of the sub-cases extracted from the
// original mega-TestE2E_ErrorResponses; behaviour is preserved.
func TestE2E_ErrorResponses_HeadNonexistent(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("does-not-exist-ever"),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
	// HEAD returns 404 but no XML body  -  SDK returns a generic error.
	respErr, ok := errors.AsType[*smithyhttp.ResponseError](err)
	if !ok {
		t.Fatalf("expected ResponseError, got %T: %v", err, err)
	}
	if respErr.HTTPStatusCode() != 404 {
		t.Errorf("HEAD status = %d, want 404", respErr.HTTPStatusCode())
	}
}

// TestE2E_ErrorResponses_DeleteNonexistent_Idempotent is one of the sub-cases extracted from the
// original mega-TestE2E_ErrorResponses; behaviour is preserved.
func TestE2E_ErrorResponses_DeleteNonexistent_Idempotent(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("does-not-exist-ever"),
	})
	if err != nil {
		t.Fatalf("DeleteObject on nonexistent key should succeed: %v", err)
	}
}

// assertS3ErrorCode checks that err contains an S3 error with the given code.
func assertS3ErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", wantCode)
	}
	// The SDK wraps S3 errors; check the error string for the code.
	if !strings.Contains(err.Error(), wantCode) {
		t.Errorf("error %q does not contain code %q", err, wantCode)
	}
}
