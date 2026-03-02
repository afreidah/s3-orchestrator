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

func TestE2E_FullLifecycle(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "e2e")
	body := []byte("end-to-end test payload")

	// PUT
	putResp, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/plain"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if putResp.ETag == nil || *putResp.ETag == "" {
		t.Error("PutObject should return an ETag")
	}

	// HEAD
	headResp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if headResp.ContentLength == nil || *headResp.ContentLength != int64(len(body)) {
		t.Errorf("HeadObject ContentLength = %v, want %d", headResp.ContentLength, len(body))
	}
	if headResp.ContentType == nil || *headResp.ContentType != "text/plain" {
		t.Errorf("HeadObject ContentType = %v, want text/plain", headResp.ContentType)
	}

	// GET
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer getResp.Body.Close()

	got, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("GetObject body = %q, want %q", got, body)
	}
	if getResp.ContentLength == nil || *getResp.ContentLength != int64(len(body)) {
		t.Errorf("GetObject ContentLength = %v, want %d", getResp.ContentLength, len(body))
	}

	// LIST
	listResp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(listResp.Contents) != 1 {
		t.Fatalf("ListObjectsV2 returned %d objects, want 1", len(listResp.Contents))
	}
	if listResp.Contents[0].Key == nil || *listResp.Contents[0].Key != key {
		t.Errorf("ListObjectsV2 key = %v, want %q", listResp.Contents[0].Key, key)
	}
	if listResp.Contents[0].Size == nil || *listResp.Contents[0].Size != int64(len(body)) {
		t.Errorf("ListObjectsV2 size = %v, want %d", listResp.Contents[0].Size, len(body))
	}

	// DELETE
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Verify deletion — GET should return NoSuchKey.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("GetObject after delete should fail")
	}
	assertS3ErrorCode(t, err, "NoSuchKey")
}

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

	// UploadPart — two 100-byte parts (sized to fit test backend quotas).
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

func TestE2E_ErrorResponses(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	t.Run("GetNonexistent", func(t *testing.T) {
		_, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String("does-not-exist-ever"),
		})
		if err == nil {
			t.Fatal("expected error for nonexistent key")
		}
		assertS3ErrorCode(t, err, "NoSuchKey")
	})

	t.Run("HeadNonexistent", func(t *testing.T) {
		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String("does-not-exist-ever"),
		})
		if err == nil {
			t.Fatal("expected error for nonexistent key")
		}
		// HEAD returns 404 but no XML body — SDK returns a generic error.
		var respErr *smithyhttp.ResponseError
		if !errors.As(err, &respErr) {
			t.Fatalf("expected ResponseError, got %T: %v", err, err)
		}
		if respErr.HTTPStatusCode() != 404 {
			t.Errorf("HEAD status = %d, want 404", respErr.HTTPStatusCode())
		}
	})

	t.Run("DeleteNonexistent_Idempotent", func(t *testing.T) {
		// S3 spec: DELETE on a nonexistent key should succeed (idempotent).
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String("does-not-exist-ever"),
		})
		if err != nil {
			t.Fatalf("DeleteObject on nonexistent key should succeed: %v", err)
		}
	})
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
