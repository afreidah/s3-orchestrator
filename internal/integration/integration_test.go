// -------------------------------------------------------------------------------
// Integration Tests - End-to-End S3 Operations
//
// Author: Alex Freidah
//
// Full-stack integration tests running against real MinIO and PostgreSQL
// containers. Covers CRUD, bucket operations, batch delete, multipart uploads
// (create, upload, complete, abort, list uploads, list parts), list objects
// (V1 and V2), quota enforcement, replication, circuit breaker failover, and
// cross-backend object operations.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"

	// -------------------------------------------------------------------------
	// CRUD
	// -------------------------------------------------------------------------
	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
)

// TestCRUD_PutGetRoundTrip is one of the sub-cases extracted from the
// original mega-TestCRUD; behaviour is preserved.
func TestCRUD_PutGetRoundTrip(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	key := uniqueKey(t, "crud")
	body := bytes.Repeat([]byte("A"), 100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

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
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// TestCRUD_PutHeadMetadata is one of the sub-cases extracted from the
// original mega-TestCRUD; behaviour is preserved.
func TestCRUD_PutHeadMetadata(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	key := uniqueKey(t, "crud")
	body := bytes.Repeat([]byte("B"), 200)

	putResp, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(200),
		ContentType:   aws.String("text/plain"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if got := aws.ToInt64(head.ContentLength); got != 200 {
		t.Errorf("ContentLength = %d, want 200", got)
	}
	if got := aws.ToString(head.ContentType); got != "text/plain" {
		t.Errorf("ContentType = %q, want %q", got, "text/plain")
	}
	if head.ETag == nil || *head.ETag == "" {
		t.Error("ETag is empty")
	}
	if putResp.ETag != nil && head.ETag != nil && *putResp.ETag != *head.ETag {
		t.Errorf("ETag mismatch: put=%q head=%q", *putResp.ETag, *head.ETag)
	}
}

// TestCRUD_PutDeleteGet404 is one of the sub-cases extracted from the
// original mega-TestCRUD; behaviour is preserved.
func TestCRUD_PutDeleteGet404(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	key := uniqueKey(t, "crud")
	body := []byte("delete-me")

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("expected error for GET after DELETE, got nil")
	}
	assertHTTPStatus(t, err, 404)
}

// TestCRUD_GetNonexistent is one of the sub-cases extracted from the
// original mega-TestCRUD; behaviour is preserved.
func TestCRUD_GetNonexistent(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	_, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fmt.Sprintf("nonexistent-%d", time.Now().UnixNano())),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent key, got nil")
	}
	assertHTTPStatus(t, err, 404)
}

// -------------------------------------------------------------------------
// QUOTA ROUTING
// -------------------------------------------------------------------------

// TestQuotaRouting_SmallObjectLandsOnFirstBackend is one of the sub-cases extracted from the
// original mega-TestQuotaRouting; behaviour is preserved.
func TestQuotaRouting_SmallObjectLandsOnFirstBackend(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	key := uniqueKey(t, "quota")
	body := bytes.Repeat([]byte("X"), 100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	backend := queryObjectBackend(t, key)
	if backend != "minio-1" {
		t.Errorf("object landed on %q, want %q", backend, "minio-1")
	}
}

// TestQuotaRouting_OverflowToSecondBackend is one of the sub-cases extracted from the
// original mega-TestQuotaRouting; behaviour is preserved.
func TestQuotaRouting_OverflowToSecondBackend(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	fillKey := uniqueKey(t, "fill")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(fillKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("F"), 900)),
		ContentLength: aws.Int64(900),
	})
	if err != nil {
		t.Fatalf("fill PutObject: %v", err)
	}

	overflowKey := uniqueKey(t, "overflow")
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(overflowKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("O"), 200)),
		ContentLength: aws.Int64(200),
	})
	if err != nil {
		t.Fatalf("overflow PutObject: %v", err)
	}

	backend := queryObjectBackend(t, overflowKey)
	if backend != "minio-2" {
		t.Errorf("overflow object landed on %q, want %q", backend, "minio-2")
	}
}

// TestQuotaRouting_AllBackendsFull507 is one of the sub-cases extracted from the
// original mega-TestQuotaRouting; behaviour is preserved.
func TestQuotaRouting_AllBackendsFull507(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "full1")),
		Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 1024)),
		ContentLength: aws.Int64(1024),
	})
	if err != nil {
		t.Fatalf("fill minio-1: %v", err)
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "full2")),
		Body:          bytes.NewReader(bytes.Repeat([]byte("B"), 2048)),
		ContentLength: aws.Int64(2048),
	})
	if err != nil {
		t.Fatalf("fill minio-2: %v", err)
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "excess")),
		Body:          bytes.NewReader([]byte("X")),
		ContentLength: aws.Int64(1),
	})
	if err == nil {
		t.Fatal("expected error when all backends full, got nil")
	}
	assertHTTPStatus(t, err, 507)
}

// TestQuotaRouting_DeleteFreesQuotaThenPutSucceeds is one of the sub-cases extracted from the
// original mega-TestQuotaRouting; behaviour is preserved.
func TestQuotaRouting_DeleteFreesQuotaThenPutSucceeds(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)

	fillKey := uniqueKey(t, "del-quota")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(fillKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("D"), 1024)),
		ContentLength: aws.Int64(1024),
	})
	if err != nil {
		t.Fatalf("fill minio-1: %v", err)
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "del-quota2")),
		Body:          bytes.NewReader(bytes.Repeat([]byte("E"), 2048)),
		ContentLength: aws.Int64(2048),
	})
	if err != nil {
		t.Fatalf("fill minio-2: %v", err)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 1024 {
		t.Fatalf("expected 1024 bytes used on minio-1, got %d", used)
	}

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fillKey),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 0 {
		t.Errorf("expected 0 bytes used after delete, got %d", used)
	}

	newKey := uniqueKey(t, "reuse")
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(newKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("N"), 500)),
		ContentLength: aws.Int64(500),
	})
	if err != nil {
		t.Fatalf("PutObject after delete: %v", err)
	}

	backend := queryObjectBackend(t, newKey)
	if backend != "minio-1" {
		t.Errorf("new object landed on %q, want %q", backend, "minio-1")
	}
}

// -------------------------------------------------------------------------
// RANGE REQUESTS
// -------------------------------------------------------------------------

// TestRangeRequests_PartialGet206 is one of the sub-cases extracted from the
// original mega-TestRangeRequests; behaviour is preserved.
func TestRangeRequests_PartialGet206(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx
	resetState(t)
	key := uniqueKey(t, "range")
	_ = key
	body := make([]byte, 256)
	_ = body
	for i := range body {
		body[i] = byte(i)
	}
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(256),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
		Range:  aws.String("bytes=0-99"),
	})
	if err != nil {
		t.Fatalf("GetObject with Range: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("got %d bytes, want 100", len(got))
	}
	if !bytes.Equal(got, body[:100]) {
		t.Error("partial body doesn't match expected range")
	}
	if resp.ContentRange == nil || *resp.ContentRange == "" {
		t.Error("expected Content-Range header in response")
	}
}

// TestRangeRequests_FullGetHasAcceptRanges is one of the sub-cases extracted from the
// original mega-TestRangeRequests; behaviour is preserved.
func TestRangeRequests_FullGetHasAcceptRanges(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx
	resetState(t)
	key := uniqueKey(t, "range")
	_ = key
	body := make([]byte, 256)
	_ = body
	for i := range body {
		body[i] = byte(i)
	}
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(256),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.AcceptRanges == nil || *resp.AcceptRanges != "bytes" {
		got := ""
		if resp.AcceptRanges != nil {
			got = *resp.AcceptRanges
		}
		t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

// TestRangeRequests_HeadHasAcceptRanges is one of the sub-cases extracted from the
// original mega-TestRangeRequests; behaviour is preserved.
func TestRangeRequests_HeadHasAcceptRanges(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx
	resetState(t)
	key := uniqueKey(t, "range")
	_ = key
	body := make([]byte, 256)
	_ = body
	for i := range body {
		body[i] = byte(i)
	}
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(256),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	resp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if resp.AcceptRanges == nil || *resp.AcceptRanges != "bytes" {
		got := ""
		if resp.AcceptRanges != nil {
			got = *resp.AcceptRanges
		}
		t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

// -------------------------------------------------------------------------
// MULTIPART UPLOAD
// -------------------------------------------------------------------------

// TestMultipartUpload verifies the multipart upload contract.
// Asserts that CreateMultipartUpload:.
func TestMultipartUpload(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "multipart")
	part1Data := bytes.Repeat([]byte("1"), 100)
	part2Data := bytes.Repeat([]byte("2"), 100)

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := create.UploadId

	up1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		UploadId:      uploadID,
		PartNumber:    aws.Int32(1),
		Body:          bytes.NewReader(part1Data),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}

	up2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		UploadId:      uploadID,
		PartNumber:    aws.Int32(2),
		Body:          bytes.NewReader(part2Data),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: up1.ETag},
				{PartNumber: aws.Int32(2), ETag: up2.ETag},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

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

	expected := append(part1Data, part2Data...)
	if !bytes.Equal(got, expected) {
		t.Errorf("assembled body mismatch: got %d bytes, want %d", len(got), len(expected))
	}
}

// -------------------------------------------------------------------------
// LIST AND COPY
// -------------------------------------------------------------------------

// TestListAndCopy_ListObjectsV2 is one of the sub-cases extracted from the
// original mega-TestListAndCopy; behaviour is preserved.
func TestListAndCopy_ListObjectsV2(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)
	prefix := fmt.Sprintf("list-test/%d/", time.Now().UnixNano())
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}

	for _, k := range keys {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader([]byte("data")),
			ContentLength: aws.Int64(4),
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	if len(list.Contents) != 3 {
		t.Errorf("got %d objects, want 3", len(list.Contents))
	}

	found := make(map[string]bool)
	for _, obj := range list.Contents {
		found[*obj.Key] = true
	}
	for _, k := range keys {
		if !found[k] {
			t.Errorf("missing key %q in list results", k)
		}
	}
}

// TestListAndCopy_CopyObject is one of the sub-cases extracted from the
// original mega-TestListAndCopy; behaviour is preserved.
func TestListAndCopy_CopyObject(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resetState(t)
	srcKey := uniqueKey(t, "copy-src")
	dstKey := uniqueKey(t, "copy-dst")
	body := []byte("copy-me-please")

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(srcKey),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject source: %v", err)
	}

	_, err = client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(virtualBucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(virtualBucket + "/" + srcKey),
	})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}

	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(dstKey),
	})
	if err != nil {
		t.Fatalf("GetObject copy: %v", err)
	}
	defer getResp.Body.Close()

	got, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("copied body mismatch: got %q, want %q", got, body)
	}
}

// delimiterGroup describes one CommonPrefix group to seed under the test
// prefix: a logical name (becomes the group's middle path component) and
// the number of objects to put inside it.
type delimiterGroup struct {
	name  string
	count int
}

// seedDelimiterGroups uploads count objects per group under the given
// prefix, naming them `{prefix}{name}/{NNN}` so they collapse into a
// single CommonPrefix (`{prefix}{name}/`) when listed with delimiter "/".
func seedDelimiterGroups(t *testing.T, ctx context.Context, client *s3.Client, prefix string, groups []delimiterGroup) {
	t.Helper()
	for _, g := range groups {
		for i := 0; i < g.count; i++ {
			key := fmt.Sprintf("%s%s/%03d", prefix, g.name, i)
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        aws.String(virtualBucket),
				Key:           aws.String(key),
				Body:          bytes.NewReader([]byte("x")),
				ContentLength: aws.Int64(1),
			})
			if err != nil {
				t.Fatalf("PutObject(%s): %v", key, err)
			}
		}
	}
}

// walkCommonPrefixes paginates ListObjectsV2 under the given prefix with
// "/" as the delimiter and returns a count of how many times each
// CommonPrefix was emitted across the paginated responses. maxKeysPerPage
// is small enough that a deep group must span store-page boundaries.
func walkCommonPrefixes(t *testing.T, ctx context.Context, client *s3.Client, prefix string, maxKeysPerPage int32) map[string]int {
	t.Helper()
	const maxPagesGuard = 10
	seen := make(map[string]int)
	var token *string
	for page := 1; page <= maxPagesGuard; page++ {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(virtualBucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			MaxKeys:           aws.Int32(maxKeysPerPage),
			ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("ListObjectsV2 page %d: %v", page, err)
		}
		for _, cp := range out.CommonPrefixes {
			seen[*cp.Prefix]++
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return seen
		}
		if out.NextContinuationToken == nil || *out.NextContinuationToken == "" {
			t.Fatal("IsTruncated=true but NextContinuationToken empty; cannot continue")
		}
		token = out.NextContinuationToken
	}
	t.Fatalf("walked %d pages without termination", maxPagesGuard)
	return nil
}

// TestListObjectsV2_DelimiterPaginationNoDuplicateCommonPrefix is the
// end-to-end regression for issue #660. It seeds a deep CommonPrefix
// group large enough to span the manager's store-page boundary, then
// walks ListObjectsV2 with a delimiter via NextContinuationToken and
// asserts no CommonPrefix appears in more than one paginated response.
//
// Drives a real Postgres + MinIO stack so the cursor rewrite is
// exercised against the production store query, not the in-memory mock.
func TestListObjectsV2_DelimiterPaginationNoDuplicateCommonPrefix(t *testing.T) {
	resetState(t)

	client := newS3Client(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("issue660/%d/", time.Now().UnixNano())

	// b/ is deep enough that paginating with maxKeys=2 must cross store
	// page boundaries mid-group, which is exactly the scenario where the
	// old NextContinuationToken would land inside b/ and the next call
	// would re-emit it.
	seedDelimiterGroups(t, ctx, client, prefix, []delimiterGroup{
		{"a", 2},
		{"b", 30},
		{"c", 2},
	})

	seen := walkCommonPrefixes(t, ctx, client, prefix, 2)

	for cp, count := range seen {
		if count > 1 {
			t.Errorf("CommonPrefix %q emitted %d times across paginated responses (want 1)", cp, count)
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		expected := prefix + name + "/"
		if seen[expected] == 0 {
			t.Errorf("expected CommonPrefix %q never appeared in any page", expected)
		}
	}
}

// TestReconcile_StaleRowSweepsCleanupQueue is the regression test for
// issue #664. Seed an object_locations row plus a cleanup_queue row that
// reference a key the backend does not actually hold; run reconcile;
// assert both rows are gone and orphan_bytes is back at zero.
//
// Drives the real Postgres + MinIO stack so the SQL transactions in
// SweepStaleCleanupQueueRows are exercised end-to-end.
func TestReconcile_StaleRowSweepsCleanupQueue(t *testing.T) {
	resetState(t)

	ctx := context.Background()
	staleKey := internalKey(uniqueKey(t, "issue664-stale"))
	const backend = "minio-1"
	// minio-1's per-test quota is 1024 bytes; keep the seed well under
	// that so RecordObject does not reject for ErrNoSpaceAvailable.
	const sizeBytes int64 = 256

	// Seed an object_locations row pointing at a key that was never
	// uploaded to the backend, plus a cleanup_queue entry for the same
	// key+backend with a corresponding orphan_bytes credit.
	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: staleKey, Backend: backend, Size: sizeBytes}); err != nil {
		t.Fatalf("seed RecordObject: %v", err)
	}
	if err := testStore.EnqueueCleanup(ctx, backend, staleKey, "test-seed", sizeBytes); err != nil {
		t.Fatalf("seed EnqueueCleanup: %v", err)
	}
	if err := testStore.IncrementOrphanBytes(ctx, backend, sizeBytes); err != nil {
		t.Fatalf("seed IncrementOrphanBytes: %v", err)
	}

	// Sanity check the seed landed.
	if got := queryObjectCopies(t, strings.TrimPrefix(staleKey, virtualBucket+"/")); got != 1 {
		t.Fatalf("seed: object_locations rows = %d, want 1", got)
	}
	if queryCleanupQueueCount(t, backend) == 0 {
		t.Fatal("seed: cleanup_queue row missing")
	}
	if got := queryOrphanBytes(t, backend); got != sizeBytes {
		t.Fatalf("seed: orphan_bytes = %d, want %d", got, sizeBytes)
	}

	// Run reconcile. Backend has no objects, DB has one stale row, so
	// the delete path fires for our seeded key.
	res, err := testReconciler.ReconcileBackend(ctx, backend, []string{virtualBucket})
	if err != nil {
		t.Fatalf("ReconcileBackend: %v", err)
	}
	if res.Removed == 0 {
		t.Errorf("reconcile removed = %d, want >= 1", res.Removed)
	}

	// Both rows must be gone after reconcile.
	if got := queryObjectCopies(t, strings.TrimPrefix(staleKey, virtualBucket+"/")); got != 0 {
		t.Errorf("object_locations rows after reconcile = %d, want 0", got)
	}
	if got := queryCleanupQueueCount(t, backend); got != 0 {
		t.Errorf("cleanup_queue rows after reconcile = %d, want 0", got)
	}
	if got := queryOrphanBytes(t, backend); got != 0 {
		t.Errorf("orphan_bytes after reconcile = %d, want 0 (decremented in step with sweep)", got)
	}
}

// TestSweepStaleCleanupQueueRows_PostgresDirect_RemovesMatchAndDecrementsOrphan
// drives the Postgres SweepStaleCleanupQueueRows method directly against
// the real DB to confirm matching rows are deleted and orphan_bytes is
// decremented by their summed size. Mirrors the SQLite-side unit test.
func TestSweepStaleCleanupQueueRows_PostgresDirect_RemovesMatchAndDecrementsOrphan(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	const backend = "minio-1"

	key := internalKey(uniqueKey(t, "sweep-direct-match"))
	mustEnqueueWithSize(t, ctx, backend, key, "test", 100)
	mustEnqueueWithSize(t, ctx, backend, key, "retry", 200)
	mustEnqueueWithSize(t, ctx, backend, internalKey(uniqueKey(t, "sweep-direct-other")), "test", 50)
	if err := testStore.IncrementOrphanBytes(ctx, backend, 350); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}

	rows, err := testStore.SweepStaleCleanupQueueRows(ctx, key, backend)
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 2 {
		t.Errorf("rows deleted = %d, want 2", rows)
	}
	if got := queryCleanupQueueCount(t, backend); got != 1 {
		t.Errorf("cleanup_queue rows = %d, want 1 (other key preserved)", got)
	}
	if got := queryOrphanBytes(t, backend); got != 50 {
		t.Errorf("orphan_bytes = %d, want 50 (350 - (100+200))", got)
	}
}

// TestSweepStaleCleanupQueueRows_PostgresDirect_NoMatchIsNoOp verifies
// the Postgres sweep returns 0 and leaves orphan_bytes untouched when no
// rows match the (key, backend) pair.
func TestSweepStaleCleanupQueueRows_PostgresDirect_NoMatchIsNoOp(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	const backend = "minio-1"

	if err := testStore.IncrementOrphanBytes(ctx, backend, 100); err != nil {
		t.Fatalf("IncrementOrphanBytes: %v", err)
	}
	rows, err := testStore.SweepStaleCleanupQueueRows(ctx, internalKey("nonexistent"), backend)
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want 0", rows)
	}
	if got := queryOrphanBytes(t, backend); got != 100 {
		t.Errorf("orphan_bytes = %d, want 100 (untouched)", got)
	}
}

// TestSweepStaleCleanupQueueRows_PostgresDirect_OnlyThisBackend verifies
// the sweep deletes only rows matching the requested backend; same-key
// entries on other backends are preserved.
func TestSweepStaleCleanupQueueRows_PostgresDirect_OnlyThisBackend(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	key := internalKey(uniqueKey(t, "sweep-direct-isolation"))
	mustEnqueueWithSize(t, ctx, "minio-1", key, "test", 100)
	mustEnqueueWithSize(t, ctx, "minio-2", key, "test", 200)

	rows, err := testStore.SweepStaleCleanupQueueRows(ctx, key, "minio-1")
	if err != nil {
		t.Fatalf("SweepStaleCleanupQueueRows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1 (only minio-1)", rows)
	}
	if got := queryCleanupQueueCount(t, "minio-2"); got != 1 {
		t.Errorf("minio-2 cleanup_queue rows = %d, want 1 (preserved)", got)
	}
}

// mustEnqueueWithSize seeds a cleanup_queue row with the given size,
// failing the test on error. Extracted to keep the per-case test bodies
// short and readable.
func mustEnqueueWithSize(t *testing.T, ctx context.Context, backend, key, reason string, size int64) {
	t.Helper()
	if err := testStore.EnqueueCleanup(ctx, backend, key, reason, size); err != nil {
		t.Fatalf("EnqueueCleanup(%s, %s, size=%d): %v", backend, key, size, err)
	}
}

// -------------------------------------------------------------------------
// SPREAD WRITE ROUTING
// -------------------------------------------------------------------------

// TestSpreadWriteRouting_DistributesAcrossBackends is one of the sub-cases extracted from the
// original mega-TestSpreadWriteRouting; behaviour is preserved.
func TestSpreadWriteRouting_DistributesAcrossBackends(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	stores := newStores(testStore)
	spreadManager := proxytest.NewManager(t, stores, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: testBackends,
			Order:    testBackendOrder,
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
		Objects:   spreadManager.Objects(),
		Multipart: spreadManager.Multipart(),
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
	spreadManager.Objects().LocationCache().Clear()

	keys := make([]string, 4)
	for i := range keys {
		keys[i] = uniqueKey(t, "spread-route")
		_, err := spreadClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader(bytes.Repeat([]byte("S"), 200)),
			ContentLength: aws.Int64(200),
		})
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
	}

	placement := make(map[string]int)
	for _, key := range keys {
		placement[queryObjectBackend(t, key)]++
	}

	t.Logf("placement: %v", placement)

	if len(placement) < 2 {
		t.Errorf("spread routing placed all objects on %v, expected distribution across 2 backends", placement)
	}

	if placement["minio-2"] < 2 {
		t.Errorf("minio-2 got %d objects, want >= 2", placement["minio-2"])
	}

	for _, key := range keys {
		resp, err := spreadClient.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject(%s): %v", key, err)
		}
		resp.Body.Close()
	}
}

// TestSpreadWriteRouting_PreferLeastUtilizedAfterImbalance is one of the sub-cases extracted from the
// original mega-TestSpreadWriteRouting; behaviour is preserved.
func TestSpreadWriteRouting_PreferLeastUtilizedAfterImbalance(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	stores := newStores(testStore)
	spreadManager := proxytest.NewManager(t, stores, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: testBackends,
			Order:    testBackendOrder,
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
		Objects:   spreadManager.Objects(),
		Multipart: spreadManager.Multipart(),
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
	spreadManager.Objects().LocationCache().Clear()

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "prefill"), Backend: "minio-1", Size: 512}); err != nil {
		t.Fatalf("RecordObject prefill: %v", err)
	}

	keys := make([]string, 3)
	for i := range keys {
		keys[i] = uniqueKey(t, "spread-imbal")
		_, err := spreadClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader(bytes.Repeat([]byte("I"), 100)),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
	}

	for i, key := range keys {
		backend := queryObjectBackend(t, key)
		if backend != "minio-2" {
			t.Errorf("obj-%d on %q, want minio-2 (least utilized)", i, backend)
		}
	}
}

// TestSpreadWriteRouting_ContrastWithPackBehavior is one of the sub-cases extracted from the
// original mega-TestSpreadWriteRouting; behaviour is preserved.
func TestSpreadWriteRouting_ContrastWithPackBehavior(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	stores := newStores(testStore)
	spreadManager := proxytest.NewManager(t, stores, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: testBackends,
			Order:    testBackendOrder,
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
		Objects:   spreadManager.Objects(),
		Multipart: spreadManager.Multipart(),
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
	spreadManager.Objects().LocationCache().Clear()
	testManager.Objects().LocationCache().Clear()

	packClient := newS3Client(t)
	packKeys := make([]string, 4)
	for i := range packKeys {
		packKeys[i] = uniqueKey(t, "pack-contrast")
		_, err := packClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(packKeys[i]),
			Body:          bytes.NewReader(bytes.Repeat([]byte("P"), 200)),
			ContentLength: aws.Int64(200),
		})
		if err != nil {
			t.Fatalf("PutObject pack %d: %v", i, err)
		}
	}

	packPlacement := make(map[string]int)
	for _, key := range packKeys {
		packPlacement[queryObjectBackend(t, key)]++
	}
	t.Logf("pack placement: %v", packPlacement)

	if packPlacement["minio-1"] != 4 {
		t.Errorf("pack placed %d on minio-1, want 4", packPlacement["minio-1"])
	}

	resetState(t)
	spreadManager.Objects().LocationCache().Clear()

	spreadKeys := make([]string, 4)
	for i := range spreadKeys {
		spreadKeys[i] = uniqueKey(t, "spread-contrast")
		_, err := spreadClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(spreadKeys[i]),
			Body:          bytes.NewReader(bytes.Repeat([]byte("S"), 200)),
			ContentLength: aws.Int64(200),
		})
		if err != nil {
			t.Fatalf("PutObject spread %d: %v", i, err)
		}
	}

	spreadPlacement := make(map[string]int)
	for _, key := range spreadKeys {
		spreadPlacement[queryObjectBackend(t, key)]++
	}
	t.Logf("spread placement: %v", spreadPlacement)

	if len(spreadPlacement) < 2 {
		t.Errorf("spread placed all on one backend: %v", spreadPlacement)
	}
}

// -------------------------------------------------------------------------
// REBALANCER
// -------------------------------------------------------------------------

// TestRebalancePackTight verifies the rebalance pack tight contract.
// Asserts that PutObject fill:.
func TestRebalancePackTight(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	ws := newWriteSet(t, client)

	// Setup: fill minio-1 to force overflow, then free space so pack can pull back.
	// Step 1: fill minio-1 completely
	fillKey := uniqueKey(t, "pack-fill")
	ws.put(ctx, fillKey, bytes.Repeat([]byte("F"), 1024))

	// Step 2: these overflow to minio-2 (minio-1 is full)
	ws.seed(ctx, "rebal-pack/obj", 3, 100)

	// Step 3: delete fill and put a smaller object so minio-1 has room
	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fillKey),
	})
	ws.drop(fillKey)
	ws.put(ctx, uniqueKey(t, "pack-refill"), bytes.Repeat([]byte("R"), 600))

	// State: minio-1=600/1024 (58.6%), minio-2=300/2048 (14.6%)
	// minio-1 is more full and has 424 bytes free, enough for 3 x 100-byte objects
	m1Before := queryQuotaUsed(t, "minio-1")
	m2Before := queryQuotaUsed(t, "minio-2")
	t.Logf("before pack: minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		m1Before, float64(m1Before)/1024*100, m2Before, float64(m2Before)/2048*100)

	packCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0,
	}

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, packCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	ws.assertIntact(ctx, "after pack")

	m1Used := queryQuotaUsed(t, "minio-1")
	m2Used := queryQuotaUsed(t, "minio-2")
	t.Logf("after pack: moved %d, minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		moved, m1Used, float64(m1Used)/1024*100, m2Used, float64(m2Used)/2048*100)

	// minio-1 should be more packed (objects pulled from minio-2)
	if moved == 0 {
		t.Error("pack should have moved objects from minio-2 into minio-1")
	}
	if m1Used <= m1Before {
		t.Errorf("minio-1 should be more packed: before=%d after=%d", m1Before, m1Used)
	}

	// Total bytes conserved
	if m1Used+m2Used != m1Before+m2Before {
		t.Errorf("total bytes_used = %d, want %d", m1Used+m2Used, m1Before+m2Before)
	}

	// No-op case: all objects already on the most-full backend
	resetState(t)
	ws.forget()

	ws.seed(ctx, "rebal-pack/noop", 5, 200)

	// minio-1 is 97.6% (1000/1024), minio-2 is 0%  -  nothing to consolidate
	movedSum, err = testWorkers.Rebalancer.Rebalance(ctx, packCfg, nil)
	moved = movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance noop: %v", err)
	}
	t.Logf("noop pack: moved %d objects", moved)
	if moved != 0 {
		t.Errorf("pack moved %d objects, want 0 (nothing to consolidate)", moved)
	}
	ws.assertIntact(ctx, "after noop pack")
}

// TestRebalancePackTinyToFuller_DestHasRoom is one of the sub-cases extracted from the
// original mega-TestRebalancePackTinyToFuller; behaviour is preserved.
func TestRebalancePackTinyToFuller_DestHasRoom(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	packCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0,
	}

	resetState(t)
	ws := newWriteSet(t, client)

	fillKey := uniqueKey(t, "tiny-fill")
	ws.put(ctx, fillKey, bytes.Repeat([]byte("F"), 1024))
	ws.put(ctx, uniqueKey(t, "tiny-big"), bytes.Repeat([]byte("B"), 1000))

	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fillKey),
	})
	ws.drop(fillKey)
	ws.put(ctx, uniqueKey(t, "tiny-obj"), bytes.Repeat([]byte("T"), 100))

	m1Before := queryQuotaUsed(t, "minio-1")
	m2Before := queryQuotaUsed(t, "minio-2")
	t.Logf("before: minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		m1Before, float64(m1Before)/1024*100, m2Before, float64(m2Before)/2048*100)

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, packCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}

	m1Used := queryQuotaUsed(t, "minio-1")
	m2Used := queryQuotaUsed(t, "minio-2")
	t.Logf("after: moved %d, minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		moved, m1Used, float64(m1Used)/1024*100, m2Used, float64(m2Used)/2048*100)

	if moved != 1 {
		t.Errorf("moved = %d, want 1", moved)
	}
	if m2Used != 1100 {
		t.Errorf("minio-2 bytes_used = %d, want 1100", m2Used)
	}
	if m1Used != 0 {
		t.Errorf("minio-1 bytes_used = %d, want 0", m1Used)
	}

	ws.assertIntact(ctx, "after pack to fuller destination")
}

// TestRebalancePackTinyToFuller_DestIsFull is one of the sub-cases extracted from the
// original mega-TestRebalancePackTinyToFuller; behaviour is preserved.
func TestRebalancePackTinyToFuller_DestIsFull(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	packCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0,
	}

	resetState(t)
	ws := newWriteSet(t, client)

	fillKey := uniqueKey(t, "full-fill")
	ws.put(ctx, fillKey, bytes.Repeat([]byte("F"), 1024))
	ws.put(ctx, uniqueKey(t, "full-big"), bytes.Repeat([]byte("B"), 2048))

	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fillKey),
	})
	ws.drop(fillKey)
	ws.put(ctx, uniqueKey(t, "full-tiny"), bytes.Repeat([]byte("T"), 100))

	t.Logf("before: minio-1=%d minio-2=%d",
		queryQuotaUsed(t, "minio-1"), queryQuotaUsed(t, "minio-2"))

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, packCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}

	t.Logf("after: moved %d, minio-1=%d minio-2=%d",
		moved, queryQuotaUsed(t, "minio-1"), queryQuotaUsed(t, "minio-2"))

	if moved != 0 {
		t.Errorf("moved = %d, want 0 (destination full)", moved)
	}
	if got := queryQuotaUsed(t, "minio-1"); got != 100 {
		t.Errorf("minio-1 bytes_used = %d, want 100", got)
	}

	ws.assertIntact(ctx, "after pack blocked by full destination")
}

// TestRebalanceSpreadEven verifies the rebalance spread even contract.
// Asserts that PutObject :.
func TestRebalanceSpreadEven(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Fill minio-1 near capacity: 5 x 200 = 1000 bytes (97.6% of 1024)
	// minio-2 is empty (0% of 2048)
	// Spread should equalize: target = 1000/3072 = 32.5%
	ws := newWriteSet(t, client)
	ws.seed(ctx, "rebal-spread/obj", 5, 200)

	// Verify initial state
	if got := queryQuotaUsed(t, "minio-1"); got != 1000 {
		t.Fatalf("minio-1 bytes_used = %d, want 1000", got)
	}
	if got := queryQuotaUsed(t, "minio-2"); got != 0 {
		t.Fatalf("minio-2 bytes_used = %d, want 0", got)
	}

	spreadCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0,
	}

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, spreadCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if moved == 0 {
		t.Fatal("spread should have moved at least one object")
	}

	// Verify utilization is more balanced
	m1Used := queryQuotaUsed(t, "minio-1")
	m2Used := queryQuotaUsed(t, "minio-2")
	m1Ratio := float64(m1Used) / 1024.0
	m2Ratio := float64(m2Used) / 2048.0

	spread := m1Ratio - m2Ratio
	if spread < 0 {
		spread = -spread
	}

	t.Logf("spread moved %d objects, minio-1=%.1f%% minio-2=%.1f%% spread=%.3f",
		moved, m1Ratio*100, m2Ratio*100, spread)

	// Target ratio is 32.5%. Best achievable with 200-byte objects:
	// 2 on minio-1 (400/1024=39.1%), 3 on minio-2 (600/2048=29.3%), spread=0.098
	// Should NOT overshoot to 1 on minio-1 (19.5%), 4 on minio-2 (39.1%), spread=0.195
	if spread > 0.15 {
		t.Errorf("utilization spread = %.3f, want < 0.15 (should not overshoot)", spread)
	}

	// Verify total bytes are conserved
	if m1Used+m2Used != 1000 {
		t.Errorf("total bytes_used = %d, want 1000", m1Used+m2Used)
	}

	// Verify all objects are still accessible
	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String("rebal-spread/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != 5 {
		t.Errorf("listed %d objects, want 5", len(list.Contents))
	}

	ws.assertIntact(ctx, "after spread")
}

// TestRebalanceSpreadAlreadyBalanced verifies the rebalance spread already balanced contract.
// Asserts that PutObject:.
func TestRebalanceSpreadAlreadyBalanced(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Put proportional data: minio-1 gets ~33% full, minio-2 gets ~33% full.
	// minio-1 limit=1024, target=333. minio-2 limit=2048, target=667.
	// Put 300 on minio-1 (29.3%), then fill minio-1 and overflow 600 to minio-2 (29.3%).
	// Both near target -> spread should do nothing.

	ws := newWriteSet(t, client)

	// 300 bytes on minio-1
	ws.put(ctx, uniqueKey(t, "bal"), bytes.Repeat([]byte("A"), 300))

	// Fill minio-1 to force overflow
	fillKey := uniqueKey(t, "bal-fill")
	ws.put(ctx, fillKey, bytes.Repeat([]byte("F"), 724))

	// Overflow 600 to minio-2 (minio-1 has 0 bytes free now)
	ws.put(ctx, uniqueKey(t, "bal-m2"), bytes.Repeat([]byte("B"), 600))

	// Delete fill to get minio-1 back to 300
	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(fillKey),
	})
	ws.drop(fillKey)

	// State: minio-1=300/1024 (29.3%), minio-2=600/2048 (29.3%)
	// Target = 900/3072 = 29.3%. Both at target already.
	m1Used := queryQuotaUsed(t, "minio-1")
	m2Used := queryQuotaUsed(t, "minio-2")
	t.Logf("state: minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		m1Used, float64(m1Used)/1024*100, m2Used, float64(m2Used)/2048*100)

	spreadCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0,
	}

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, spreadCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	t.Logf("moved %d objects", moved)
	if moved != 0 {
		t.Errorf("spread moved %d, want 0 (already balanced)", moved)
	}

	ws.assertIntact(ctx, "after balanced spread")
}

// TestRebalanceSpreadOversizedObject verifies the rebalance spread oversized object contract.
// Asserts that PutObject big:.
func TestRebalanceSpreadOversizedObject(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Put one large object (800 bytes) on minio-1 and a small one (200 bytes).
	// Target = 1000/3072 = 32.5%. minio-1 excess = 1000 - 333 = 667 bytes.
	// The 800-byte object is larger than the 667-byte excess, so spread should
	// only move the 200-byte object.
	ws := newWriteSet(t, client)
	ws.put(ctx, uniqueKey(t, "big"), bytes.Repeat([]byte("B"), 800))
	ws.put(ctx, uniqueKey(t, "small"), bytes.Repeat([]byte("S"), 200))

	// State: minio-1=1000/1024 (97.6%), minio-2=0/2048 (0%)
	spreadCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0,
	}

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, spreadCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}

	m1Used := queryQuotaUsed(t, "minio-1")
	m2Used := queryQuotaUsed(t, "minio-2")
	t.Logf("moved %d, minio-1=%d (%.1f%%) minio-2=%d (%.1f%%)",
		moved, m1Used, float64(m1Used)/1024*100, m2Used, float64(m2Used)/2048*100)

	// Only the 200-byte object should move (800 > excess of 667)
	if moved != 1 {
		t.Errorf("moved = %d, want 1 (only small object fits excess)", moved)
	}
	if m1Used != 800 {
		t.Errorf("minio-1 = %d, want 800 (big object stays)", m1Used)
	}
	if m2Used != 200 {
		t.Errorf("minio-2 = %d, want 200 (small object moved)", m2Used)
	}

	ws.assertIntact(ctx, "after oversized-object spread")
}

// TestRebalanceSpreadStableAcrossCycles verifies the rebalance spread stable across cycles contract.
// Asserts that PutObject :.
func TestRebalanceSpreadStableAcrossCycles(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Setup: 5 x 200-byte objects on minio-1
	ws := newWriteSet(t, client)
	ws.seed(ctx, "stable/obj", 5, 200)

	spreadCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0,
	}

	// Cycle 1
	moved1Sum, err := testWorkers.Rebalancer.Rebalance(ctx, spreadCfg, nil)
	moved1 := moved1Sum.Succeeded
	if err != nil {
		t.Fatalf("Cycle 1: %v", err)
	}
	m1After1 := queryQuotaUsed(t, "minio-1")
	m2After1 := queryQuotaUsed(t, "minio-2")
	t.Logf("cycle 1: moved %d, minio-1=%d minio-2=%d", moved1, m1After1, m2After1)

	// Cycle 2  -  should be a no-op, nothing bounces
	moved2Sum, err := testWorkers.Rebalancer.Rebalance(ctx, spreadCfg, nil)
	moved2 := moved2Sum.Succeeded
	if err != nil {
		t.Fatalf("Cycle 2: %v", err)
	}
	m1After2 := queryQuotaUsed(t, "minio-1")
	m2After2 := queryQuotaUsed(t, "minio-2")
	t.Logf("cycle 2: moved %d, minio-1=%d minio-2=%d", moved2, m1After2, m2After2)

	if moved2 != 0 {
		t.Errorf("cycle 2 moved %d objects, want 0 (should be stable)", moved2)
	}
	if m1After2 != m1After1 || m2After2 != m2After1 {
		t.Errorf("state changed between cycles: before=(%d,%d) after=(%d,%d)",
			m1After1, m2After1, m1After2, m2After2)
	}

	ws.assertIntact(ctx, "after two spread cycles")
}

// TestRebalanceSpreadBatchLimited verifies the rebalance spread batch limited contract.
// Asserts that PutObject :.
func TestRebalanceSpreadBatchLimited(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// 5 x 100-byte objects on minio-1. With batch_size=2, it takes
	// multiple cycles. No object should move twice.
	ws := newWriteSet(t, client)
	ws.seed(ctx, "batch/obj", 5, 100)

	// State: minio-1=500/1024 (48.8%), minio-2=0/2048 (0%)
	// Target = 500/3072 = 16.3%. minio-1 target = 167, excess = 333.
	// With 100-byte objects, can move 3 (300 <= 333). batch_size=2 limits to 2 per cycle.
	smallBatchCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 2,
		Threshold: 0,
	}

	// Cycle 1: moves 2
	moved1Sum, err := testWorkers.Rebalancer.Rebalance(ctx, smallBatchCfg, nil)
	moved1 := moved1Sum.Succeeded
	if err != nil {
		t.Fatalf("Cycle 1: %v", err)
	}
	m1After1 := queryQuotaUsed(t, "minio-1")
	m2After1 := queryQuotaUsed(t, "minio-2")
	t.Logf("cycle 1: moved %d, minio-1=%d minio-2=%d", moved1, m1After1, m2After1)

	if moved1 != 2 {
		t.Errorf("cycle 1 moved %d, want 2 (batch limited)", moved1)
	}

	// Cycle 2: moves remaining needed
	moved2Sum, err := testWorkers.Rebalancer.Rebalance(ctx, smallBatchCfg, nil)
	moved2 := moved2Sum.Succeeded
	if err != nil {
		t.Fatalf("Cycle 2: %v", err)
	}
	m1After2 := queryQuotaUsed(t, "minio-1")
	m2After2 := queryQuotaUsed(t, "minio-2")
	t.Logf("cycle 2: moved %d, minio-1=%d minio-2=%d", moved2, m1After2, m2After2)

	// Total bytes always conserved
	if m1After2+m2After2 != 500 {
		t.Errorf("total bytes = %d, want 500", m1After2+m2After2)
	}

	// Cycle 3: should stabilize
	moved3Sum, err := testWorkers.Rebalancer.Rebalance(ctx, smallBatchCfg, nil)
	moved3 := moved3Sum.Succeeded
	if err != nil {
		t.Fatalf("Cycle 3: %v", err)
	}
	t.Logf("cycle 3: moved %d, minio-1=%d minio-2=%d",
		moved3, queryQuotaUsed(t, "minio-1"), queryQuotaUsed(t, "minio-2"))

	// Eventually no more moves
	totalMoved := moved1 + moved2 + moved3
	t.Logf("total moved across 3 cycles: %d", totalMoved)

	// Should not have moved more objects than exist (no bouncing)
	if totalMoved > 5 {
		t.Errorf("total moved = %d, want <= 5 (objects should not bounce)", totalMoved)
	}

	ws.assertIntact(ctx, "after batch-limited spread")
}

// TestRebalanceThresholdSkip verifies the rebalance threshold skip contract.
// Asserts that PutObject 1:.
func TestRebalanceThresholdSkip(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	ws := newWriteSet(t, client)

	// PUT a small object on each backend to create roughly balanced usage
	ws.put(ctx, uniqueKey(t, "threshold"), bytes.Repeat([]byte("T"), 100))

	// Fill minio-1 so it overflows to minio-2
	ws.put(ctx, uniqueKey(t, "threshold-fill"), bytes.Repeat([]byte("F"), 1000))

	// Put another small object that lands on minio-2
	ws.put(ctx, uniqueKey(t, "threshold2"), bytes.Repeat([]byte("U"), 200))

	// With a high threshold, rebalance should skip
	skipCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0.99, // extremely high threshold
	}

	movedSum, err := testWorkers.Rebalancer.Rebalance(ctx, skipCfg, nil)
	moved := movedSum.Succeeded
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if moved != 0 {
		t.Errorf("expected 0 moves with high threshold, got %d", moved)
	}

	ws.assertIntact(ctx, "after threshold-skipped rebalance")
}

// -------------------------------------------------------------------------
// REPLICATION
// -------------------------------------------------------------------------

// TestReplicationBasic verifies the replication basic contract.
// Asserts that PutObject:.
func TestReplicationBasic(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "repl-basic")
	body := bytes.Repeat([]byte("R"), 100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Should have 1 copy initially
	if copies := queryObjectCopies(t, key); copies != 1 {
		t.Fatalf("expected 1 copy, got %d", copies)
	}

	// Run replication with factor=2
	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	replSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if replSum.CopiesCreated != 1 {
		t.Errorf("created = %d, want 1", replSum.CopiesCreated)
	}

	// Should have 2 copies on different backends
	if copies := queryObjectCopies(t, key); copies != 2 {
		t.Errorf("expected 2 copies, got %d", copies)
	}
	backends := queryObjectBackends(t, key)
	if len(backends) != 2 || backends[0] == backends[1] {
		t.Errorf("expected 2 different backends, got %v", backends)
	}

	// GET still works
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after replication: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch after replication")
	}
}

// TestReplicationOverwrite verifies the replication overwrite contract.
// Asserts that PutObject v1:.
func TestReplicationOverwrite(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "repl-overwrite")
	body1 := bytes.Repeat([]byte("A"), 100)
	body2 := bytes.Repeat([]byte("B"), 150)

	// PUT original
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body1),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject v1: %v", err)
	}

	// Replicate to 2 copies
	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate v1: %v", err)
	}
	if copies := queryObjectCopies(t, key); copies != 2 {
		t.Fatalf("expected 2 copies after first replication, got %d", copies)
	}

	// Overwrite with new content
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body2),
		ContentLength: aws.Int64(150),
	})
	if err != nil {
		t.Fatalf("PutObject v2: %v", err)
	}

	// Old replicas should be gone, only 1 new copy
	if copies := queryObjectCopies(t, key); copies != 1 {
		t.Errorf("expected 1 copy after overwrite, got %d", copies)
	}

	// Replicate again
	replSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate v2: %v", err)
	}
	if replSum.CopiesCreated != 1 {
		t.Errorf("created = %d, want 1", replSum.CopiesCreated)
	}

	// Verify new content on GET
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body2) {
		t.Errorf("body mismatch: got %d bytes of %q, want %d bytes of %q",
			len(got), got[:1], len(body2), body2[:1])
	}
}

// TestReplicationDelete verifies the replication delete contract.
// Asserts that PutObject:.
func TestReplicationDelete(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "repl-delete")
	body := bytes.Repeat([]byte("D"), 100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Replicate to 2 copies
	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %v", backends)
	}

	// Record quota before delete
	m1Before := queryQuotaUsed(t, "minio-1")
	m2Before := queryQuotaUsed(t, "minio-2")

	// DELETE via proxy
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// All copies should be gone
	if copies := queryObjectCopies(t, key); copies != 0 {
		t.Errorf("expected 0 copies after delete, got %d", copies)
	}

	// Quota should be decremented on both backends
	m1After := queryQuotaUsed(t, "minio-1")
	m2After := queryQuotaUsed(t, "minio-2")
	totalFreed := (m1Before - m1After) + (m2Before - m2After)
	if totalFreed != 200 { // 100 bytes on each backend
		t.Errorf("expected 200 bytes freed total, got %d", totalFreed)
	}
}

// TestReplicationReadFailover verifies the replication read failover contract.
// Asserts that PutObject:.
func TestReplicationReadFailover(t *testing.T) {
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "repl-failover")
	body := bytes.Repeat([]byte("F"), 100)

	// PUT via manager (not proxy, we need direct backend access below)
	client := newS3Client(t)
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Replicate to 2 copies
	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %v", backends)
	}

	// Delete the primary copy directly from its MinIO backend (bypass proxy)
	primaryBackend := backends[0]
	deleteDirectFromMinio(t, primaryBackend, key)

	// GET via proxy should still succeed (failover to replica)
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject should failover to replica: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch after failover")
	}
}

// TestReplicationAlreadyReplicated verifies the replication already replicated contract.
// Asserts that PutObject:.
func TestReplicationAlreadyReplicated(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "repl-noop")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("N"), 100)),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}

	// First replication
	firstSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate 1: %v", err)
	}
	if firstSum.CopiesCreated != 1 {
		t.Errorf("first replicate created = %d, want 1", firstSum.CopiesCreated)
	}

	// Second replication  -  should be a no-op
	secondSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate 2: %v", err)
	}
	if secondSum.CopiesCreated != 0 {
		t.Errorf("second replicate created = %d, want 0", secondSum.CopiesCreated)
	}
}

// TestReplicationNoSpace verifies the replication no space contract.
// Asserts that fill minio-1:.
func TestReplicationNoSpace(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Fill both backends to capacity
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "fill1")),
		Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 1024)),
		ContentLength: aws.Int64(1024),
	})
	if err != nil {
		t.Fatalf("fill minio-1: %v", err)
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "fill2")),
		Body:          bytes.NewReader(bytes.Repeat([]byte("B"), 2048)),
		ContentLength: aws.Int64(2048),
	})
	if err != nil {
		t.Fatalf("fill minio-2: %v", err)
	}

	// Each object has 1 copy, factor=2 means they need replicas,
	// but the other backend is full  -  graceful degradation
	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	replSum, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if replSum.CopiesCreated != 0 {
		t.Errorf("created = %d, want 0 (no space for replicas)", replSum.CopiesCreated)
	}
}

// TestRebalancerWithReplicas verifies the rebalancer with replicas contract.
// Asserts that PutObject:.
func TestRebalancerWithReplicas(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	// Put an object and replicate it to 2 copies
	ws := newWriteSet(t, client)
	key := uniqueKey(t, "rebal-repl")
	ws.put(ctx, key, bytes.Repeat([]byte("X"), 100))

	replCfg := config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err := testWorkers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 2 {
		t.Fatalf("expected 2 copies, got %v", backends)
	}

	// Run rebalancer  -  it should not place 2 copies on the same backend
	rebalCfg := config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "spread",
		BatchSize: 10,
		Threshold: 0,
	}
	_, err = testWorkers.Rebalancer.Rebalance(ctx, rebalCfg, nil)
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}

	// Verify copies are still on different backends
	backendsAfter := queryObjectBackends(t, key)
	if len(backendsAfter) < 1 {
		t.Fatal("object lost all copies")
	}
	seen := make(map[string]bool)
	for _, b := range backendsAfter {
		if seen[b] {
			t.Errorf("duplicate backend %q  -  rebalancer placed 2 copies on same backend", b)
		}
		seen[b] = true
	}

	ws.assertIntact(ctx, "after rebalance with replicas")
}

// -------------------------------------------------------------------------
// OVER-REPLICATION CLEANUP
// -------------------------------------------------------------------------

// TestOverReplicationBasic verifies the over replication basic contract.
// Asserts that PutObject:.
func TestOverReplicationBasic(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	_, workers := newThreeBackendManager(t)

	key := uniqueKey(t, "overrepl-basic")
	body := bytes.Repeat([]byte("O"), 100)

	// PUT object -> 1 copy
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Replicate to factor=3 -> 3 copies across 3 backends
	replCfg := config.ReplicationConfig{
		Factor:         3,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = workers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate to factor 3: %v", err)
	}
	if copies := queryObjectCopies(t, key); copies != 3 {
		t.Fatalf("expected 3 copies after replication, got %d", copies)
	}

	// Over-replication cleanup with factor=3 should be a no-op
	cleanSum, err := workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      3,
		BatchSize:   50,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean (at factor): %v", err)
	}
	if cleanSum.CopiesRemoved != 0 {
		t.Errorf("expected 0 removed when at factor, got %d", cleanSum.CopiesRemoved)
	}

	// Now lower the factor to 2 -> object is over-replicated
	cleanSum, err = workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   50,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean (over factor): %v", err)
	}
	if cleanSum.CopiesRemoved != 1 {
		t.Errorf("expected 1 removed, got %d", cleanSum.CopiesRemoved)
	}

	// Should have exactly 2 copies remaining
	if copies := queryObjectCopies(t, key); copies != 2 {
		t.Errorf("expected 2 copies after cleanup, got %d", copies)
	}

	// GET still returns correct content
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after cleanup: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch after over-replication cleanup")
	}
}

// TestOverReplicationMultipleObjects verifies the over replication multiple objects contract.
// Asserts that PutObject :.
func TestOverReplicationMultipleObjects(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	_, workers := newThreeBackendManager(t)

	keys := make([]string, 3)
	for i := range keys {
		keys[i] = uniqueKey(t, fmt.Sprintf("overrepl-multi-%d", i))
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader(bytes.Repeat([]byte("M"), 50)),
			ContentLength: aws.Int64(50),
		})
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
	}

	// Replicate all to factor=3
	replCfg := config.ReplicationConfig{
		Factor:         3,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err := workers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	for i, key := range keys {
		if c := queryObjectCopies(t, key); c != 3 {
			t.Fatalf("key %d: expected 3 copies, got %d", i, c)
		}
	}

	// Clean with factor=2 -> each object loses 1 copy
	cleanSum, err := workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   50,
		Concurrency: 2,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if cleanSum.CopiesRemoved != 3 {
		t.Errorf("expected 3 removed, got %d", cleanSum.CopiesRemoved)
	}

	for i, key := range keys {
		if c := queryObjectCopies(t, key); c != 2 {
			t.Errorf("key %d: expected 2 copies after cleanup, got %d", i, c)
		}
	}
}

// TestOverReplicationDrainingBackendRemovedFirst verifies the over replication draining backend removed first contract.
// Asserts that PutObject:.
func TestOverReplicationDrainingBackendRemovedFirst(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	mgr, workers := newThreeBackendManager(t)

	key := uniqueKey(t, "overrepl-drain")
	body := bytes.Repeat([]byte("D"), 100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Replicate to factor=3 -> copies on all 3 backends
	replCfg := config.ReplicationConfig{
		Factor:         3,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = workers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 3 {
		t.Fatalf("expected 3 backends, got %v", backends)
	}

	// Mark the first backend draining so the cleaner scores its copy lowest,
	// without launching the real drain goroutine -- that would race the
	// explicit Clean below and remove the copy itself.
	drainTarget := backends[0]
	mgr.Drain().SeedActiveForTest(drainTarget)
	defer mgr.Drain().ClearState()

	// Clean with factor=2 -> should remove 1 copy, preferring the draining backend (score 0)
	cleanSum, err := workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   50,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if cleanSum.CopiesRemoved != 1 {
		t.Errorf("expected 1 removed, got %d", cleanSum.CopiesRemoved)
	}

	// The draining backend's copy should have been removed
	remaining := queryObjectBackends(t, key)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 copies, got %v", remaining)
	}
	for _, b := range remaining {
		if b == drainTarget {
			t.Errorf("draining backend %s should have been removed, but it still has a copy", drainTarget)
		}
	}

	// A GET fails over, so checking the proxy alone would not notice a bad
	// survivor. Read every remaining copy.
	assertObjectIntact(t, ctx, client, key, body, "after drain cleanup")
}

// TestOverReplicationQuotaFreed verifies the over replication quota freed contract.
// Asserts that PutObject:.
func TestOverReplicationQuotaFreed(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	_, workers := newThreeBackendManager(t)

	key := uniqueKey(t, "overrepl-quota")
	size := int64(100)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("Q"), int(size))),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Replicate to factor=3
	replCfg := config.ReplicationConfig{
		Factor:         3,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = workers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 3 {
		t.Fatalf("expected 3 backends, got %v", backends)
	}

	// Record quota before cleanup
	quotaBefore := make(map[string]int64)
	for _, b := range backends {
		quotaBefore[b] = queryQuotaUsed(t, b)
	}

	// Clean with factor=2 -> remove 1 excess copy
	cleanSum, err := workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   50,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if cleanSum.CopiesRemoved != 1 {
		t.Errorf("expected 1 removed, got %d", cleanSum.CopiesRemoved)
	}

	// Find which backend lost its copy
	remaining := queryObjectBackends(t, key)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 copies remaining, got %v", remaining)
	}

	remainingSet := make(map[string]bool)
	for _, b := range remaining {
		remainingSet[b] = true
	}
	var removedBackend string
	for _, b := range backends {
		if !remainingSet[b] {
			removedBackend = b
			break
		}
	}

	// Quota on the removed backend should have decreased
	quotaAfter := queryQuotaUsed(t, removedBackend)
	if quotaAfter >= quotaBefore[removedBackend] {
		t.Errorf("expected quota to decrease on %s: before=%d, after=%d",
			removedBackend, quotaBefore[removedBackend], quotaAfter)
	}
}

// TestOverReplicationCountPending verifies the over replication count pending contract.
// Asserts that PutObject:.
func TestOverReplicationCountPending(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	_, workers := newThreeBackendManager(t)

	key := uniqueKey(t, "overrepl-count")

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(bytes.Repeat([]byte("C"), 100)),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// No over-replication with factor=3
	count, err := workers.OverReplicationCleaner.CountPending(ctx, 3)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 pending with 1 copy and factor 3, got %d", count)
	}

	// Replicate to 3 copies
	replCfg := config.ReplicationConfig{
		Factor:         3,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	_, err = workers.Replicator.Replicate(ctx, replCfg, nil)
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	// 3 copies with factor=3 -> not over-replicated
	count, err = workers.OverReplicationCleaner.CountPending(ctx, 3)
	if err != nil {
		t.Fatalf("CountPending factor=3: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 pending at factor, got %d", count)
	}

	// 3 copies with factor=2 -> over-replicated
	count, err = workers.OverReplicationCleaner.CountPending(ctx, 2)
	if err != nil {
		t.Fatalf("CountPending factor=2: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 pending when over factor, got %d", count)
	}

	// Clean it and verify count drops
	_, err = workers.OverReplicationCleaner.Clean(ctx, config.ReplicationConfig{
		Factor:      2,
		BatchSize:   50,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}

	count, err = workers.OverReplicationCleaner.CountPending(ctx, 2)
	if err != nil {
		t.Fatalf("CountPending after clean: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 pending after cleanup, got %d", count)
	}
}

// -------------------------------------------------------------------------
// IMPORT (Sync)
// -------------------------------------------------------------------------

// TestImportPreExistingObjects_ImportAndAccessViaProxy is one of the sub-cases extracted from the
// original mega-TestImportPreExistingObjects; behaviour is preserved.
func TestImportPreExistingObjects_ImportAndAccessViaProxy(t *testing.T) {
	ctx := context.Background()
	resetState(t)

	keys := seedDirectMinioObjects(t, ctx, "import-test/obj", 3, 100, "I")
	proxyClient := newS3Client(t)
	assertProxy404ForAll(t, ctx, proxyClient, keys)
	importAllToMinio1(t, ctx, keys, 100)

	if used := queryQuotaUsed(t, "minio-1"); used != 300 {
		t.Errorf("minio-1 bytes_used = %d, want 300", used)
	}
	assertProxyServesAll(t, ctx, proxyClient, keys, 100)

	list, err := proxyClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String("import-test/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != 3 {
		t.Errorf("listed %d objects, want 3", len(list.Contents))
	}
}

// seedDirectMinioObjects writes count objects directly to the minio-1
// container (bypassing the proxy) under a unique prefix derived from
// keyPrefix. Returns the user-visible key list.
func seedDirectMinioObjects(t *testing.T, ctx context.Context, keyPrefix string, count int, sizeBytes int, fillByte string) []string {
	t.Helper()
	directClient := newDirectMinioClient(t, "minio-1")
	keys := make([]string, count)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s-%d-%d", keyPrefix, i, time.Now().UnixNano())
		_, err := directClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String("backend1"),
			Key:           aws.String(internalKey(keys[i])),
			Body:          bytes.NewReader(bytes.Repeat([]byte(fillByte), sizeBytes)),
			ContentLength: aws.Int64(int64(sizeBytes)),
		})
		if err != nil {
			t.Fatalf("direct PutObject(%s): %v", keys[i], err)
		}
	}
	return keys
}

// assertProxy404ForAll fetches each key through the proxy and fails the
// test if any request does not return 404.
func assertProxy404ForAll(t *testing.T, ctx context.Context, proxyClient *s3.Client, keys []string) {
	t.Helper()
	for _, key := range keys {
		_, err := proxyClient.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err == nil {
			t.Fatalf("expected 404 for %q before import, got nil", key)
		}
		assertHTTPStatus(t, err, 404)
	}
}

// importAllToMinio1 imports each key into the metadata store as living
// on minio-1 with the provided size, asserting that ImportObject reports
// each row as freshly inserted.
func importAllToMinio1(t *testing.T, ctx context.Context, keys []string, sizeBytes int64) {
	t.Helper()
	for _, key := range keys {
		outcome, err := testStore.ImportObject(ctx, internalKey(key), "minio-1", sizeBytes, false, nil)
		if err != nil {
			t.Fatalf("ImportObject(%q): %v", key, err)
		}
		if outcome != core.ImportInserted {
			t.Errorf("ImportObject(%q) = %s, want inserted", key, outcome)
		}
	}
}

// assertProxyServesAll fetches each key through the proxy and asserts
// the returned body is exactly wantSize bytes.
func assertProxyServesAll(t *testing.T, ctx context.Context, proxyClient *s3.Client, keys []string, wantSize int) {
	t.Helper()
	for _, key := range keys {
		resp, err := proxyClient.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject(%q) after import: %v", key, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(got) != wantSize {
			t.Errorf("GetObject(%q) body = %d bytes, want %d", key, len(got), wantSize)
		}
	}
}

// TestImportPreExistingObjects_ImportIdempotent is one of the sub-cases extracted from the
// original mega-TestImportPreExistingObjects; behaviour is preserved.
func TestImportPreExistingObjects_ImportIdempotent(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	directClient := newDirectMinioClient(t, "minio-1")
	key := fmt.Sprintf("import-idem/%d", time.Now().UnixNano())
	_, err := directClient.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String("backend1"),
		Key:           aws.String(internalKey(key)),
		Body:          bytes.NewReader(bytes.Repeat([]byte("D"), 200)),
		ContentLength: aws.Int64(200),
	})
	if err != nil {
		t.Fatalf("direct PutObject: %v", err)
	}

	store := testStore

	imported, err := store.ImportObject(ctx, internalKey(key), "minio-1", 200, false, nil)
	if err != nil {
		t.Fatalf("ImportObject first: %v", err)
	}
	if imported != core.ImportInserted {
		t.Errorf("first ImportObject = %s, want inserted", imported)
	}

	imported, err = store.ImportObject(ctx, internalKey(key), "minio-1", 200, false, nil)
	if err != nil {
		t.Fatalf("ImportObject second: %v", err)
	}
	if imported != core.ImportSkippedExisting {
		t.Errorf("second ImportObject = %s, want skipped_existing (idempotent skip)", imported)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 200 {
		t.Errorf("minio-1 bytes_used = %d, want 200 (not double-counted)", used)
	}

	if copies := queryObjectCopies(t, key); copies != 1 {
		t.Errorf("object copies = %d, want 1", copies)
	}
}

// TestImportPreExistingObjects_ImportDoesNotOverwriteProxyObject is one of the sub-cases extracted from the
// original mega-TestImportPreExistingObjects; behaviour is preserved.
func TestImportPreExistingObjects_ImportDoesNotOverwriteProxyObject(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	proxyClient := newS3Client(t)
	key := uniqueKey(t, "import-existing")
	body := bytes.Repeat([]byte("P"), 150)
	_, err := proxyClient.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(150),
	})
	if err != nil {
		t.Fatalf("PutObject via proxy: %v", err)
	}

	backend := queryObjectBackend(t, key)

	store := testStore
	imported, err := store.ImportObject(ctx, internalKey(key), backend, 150, false, nil)
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	if imported != core.ImportSkippedExisting {
		t.Errorf("ImportObject = %s, want it to skip an existing proxy object", imported)
	}

	if used := queryQuotaUsed(t, backend); used != 150 {
		t.Errorf("%s bytes_used = %d, want 150", backend, used)
	}
}

// TestListObjectsFromBackend verifies the list objects from backend contract.
// Asserts that direct PutObject():.
func TestListObjectsFromBackend(t *testing.T) {
	ctx := context.Background()
	resetState(t)

	// Put objects directly on MinIO
	directClient := newDirectMinioClient(t, "minio-1")
	prefix := fmt.Sprintf("list-backend/%d/", time.Now().UnixNano())
	keys := []string{prefix + "aaa", prefix + "bbb", prefix + "ccc"}

	for _, key := range keys {
		_, err := directClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String("backend1"),
			Key:           aws.String(key),
			Body:          bytes.NewReader(bytes.Repeat([]byte("L"), 50)),
			ContentLength: aws.Int64(50),
		})
		if err != nil {
			t.Fatalf("direct PutObject(%s): %v", key, err)
		}
	}

	// Use S3Backend.ListObjects to scan the bucket
	backend, err := s3be.NewS3Backend(context.Background(), &config.BackendConfig{
		Name:            "minio-1",
		Endpoint:        envOrDefault("MINIO1_ENDPOINT", "http://localhost:19000"),
		Region:          "us-east-1",
		Bucket:          "backend1",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}

	var listed []s3be.ListedObject
	err = backend.ListObjects(ctx, prefix, func(page []s3be.ListedObject) error {
		listed = append(listed, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	if len(listed) != 3 {
		t.Errorf("ListObjects returned %d objects, want 3", len(listed))
	}

	found := make(map[string]bool)
	for _, obj := range listed {
		found[obj.Key] = true
		if obj.SizeBytes != 50 {
			t.Errorf("object %q size = %d, want 50", obj.Key, obj.SizeBytes)
		}
	}
	for _, key := range keys {
		if !found[key] {
			t.Errorf("missing key %q in ListObjects results", key)
		}
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// newDirectMinioClient creates an S3 client pointed directly at a MinIO backend,
// bypassing the proxy. Used for placing objects that the proxy doesn't know about.
func newDirectMinioClient(t *testing.T, backendName string) *s3.Client {
	t.Helper()

	endpoints := map[string]string{
		"minio-1": envOrDefault("MINIO1_ENDPOINT", "http://localhost:19000"),
		"minio-2": envOrDefault("MINIO2_ENDPOINT", "http://localhost:19002"),
	}

	endpoint, ok := endpoints[backendName]
	if !ok {
		t.Fatalf("unknown backend %q", backendName)
	}

	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
		UsePathStyle: true,
	})
}

// deleteDirectFromMinio deletes an object directly from a MinIO backend,
// bypassing the proxy. Used to simulate a backend failure for failover tests.
func deleteDirectFromMinio(t *testing.T, backendName, key string) {
	t.Helper()

	buckets := map[string]string{
		"minio-1": "backend1",
		"minio-2": "backend2",
	}

	directClient := newDirectMinioClient(t, backendName)
	_, err := directClient.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(buckets[backendName]),
		Key:    aws.String(internalKey(key)),
	})
	if err != nil {
		t.Fatalf("direct delete from %s: %v", backendName, err)
	}
}

// assertHTTPStatus checks that the error contains the expected HTTP status code.
func assertHTTPStatus(t *testing.T, err error, wantStatus int) {
	t.Helper()
	if respErr, ok := errors.AsType[*smithyhttp.ResponseError](err); ok {
		if respErr.HTTPStatusCode() != wantStatus {
			t.Errorf("HTTP status = %d, want %d", respErr.HTTPStatusCode(), wantStatus)
		}
		return
	}
	// If we can't extract the HTTP status, just note the error type
	t.Logf("could not extract HTTP status from error (type %T): %v", err, err)
}

// -------------------------------------------------------------------------
// STORE-LEVEL TESTS
// -------------------------------------------------------------------------

// TestStore_RecordObject_OverwriteUpdatesQuota is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_RecordObject_OverwriteUpdatesQuota(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	key := uniqueKey(t, "store-overwrite")

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: internalKey(key), Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject A: %v", err)
	}
	if used := queryQuotaUsed(t, "minio-1"); used != 100 {
		t.Fatalf("minio-1 after first record = %d, want 100", used)
	}

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: internalKey(key), Backend: "minio-2", Size: 200}); err != nil {
		t.Fatalf("RecordObject B: %v", err)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 0 {
		t.Errorf("minio-1 after overwrite = %d, want 0", used)
	}

	if used := queryQuotaUsed(t, "minio-2"); used != 200 {
		t.Errorf("minio-2 after overwrite = %d, want 200", used)
	}

	if copies := queryObjectCopies(t, key); copies != 1 {
		t.Errorf("copies = %d, want 1", copies)
	}

	if backend := queryObjectBackend(t, key); backend != "minio-2" {
		t.Errorf("backend = %q, want %q", backend, "minio-2")
	}
}

// TestStore_DeleteObject_NotFound is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_DeleteObject_NotFound(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	_, err := testStore.DeleteObject(ctx, "nonexistent-key-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	s3Err, ok := errors.AsType[*core.S3Error](err)
	if !ok {
		t.Fatalf("expected *S3Error, got %T: %v", err, err)
	}
	if s3Err.StatusCode != 404 {
		t.Errorf("status = %d, want 404", s3Err.StatusCode)
	}
}

// TestStore_MoveObjectLocation_RaceSafe is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_MoveObjectLocation_RaceSafe(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	key := uniqueKey(t, "store-move")

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	size, err := testStore.MoveObjectLocation(ctx, key, "minio-1", "minio-2")
	if err != nil {
		t.Fatalf("MoveObjectLocation: %v", err)
	}
	if size != 100 {
		t.Errorf("moved size = %d, want 100", size)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 0 {
		t.Errorf("minio-1 after move = %d, want 0", used)
	}
	if used := queryQuotaUsed(t, "minio-2"); used != 100 {
		t.Errorf("minio-2 after move = %d, want 100", used)
	}

	size, err = testStore.MoveObjectLocation(ctx, key, "minio-1", "minio-2")
	if err != nil {
		t.Fatalf("second MoveObjectLocation: %v", err)
	}
	if size != 0 {
		t.Errorf("second move size = %d, want 0 (source gone)", size)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 0 {
		t.Errorf("minio-1 after second move = %d, want 0", used)
	}
	if used := queryQuotaUsed(t, "minio-2"); used != 100 {
		t.Errorf("minio-2 after second move = %d, want 100", used)
	}
}

// TestStore_ListObjects_PaginationAndEscaping is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_ListObjects_PaginationAndEscaping(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	prefix := fmt.Sprintf("list-escape/%d/", time.Now().UnixNano())
	wildcardKeys := []string{
		prefix + "normal-key",
		prefix + "has%percent",
		prefix + "has_underscore",
	}
	for _, key := range wildcardKeys {
		if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-1", Size: 10}); err != nil {
			t.Fatalf("RecordObject(%q): %v", key, err)
		}
	}

	result, err := testStore.ListObjects(ctx, prefix, "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Objects) != 3 {
		t.Errorf("got %d objects, want 3", len(result.Objects))
	}

	underscorePrefix := prefix + "has_"
	result, err = testStore.ListObjects(ctx, underscorePrefix, "", 1000)
	if err != nil {
		t.Fatalf("ListObjects underscore: %v", err)
	}
	if len(result.Objects) != 1 {
		t.Errorf("underscore prefix got %d objects, want 1", len(result.Objects))
	}

	result, err = testStore.ListObjects(ctx, prefix, "", 2)
	if err != nil {
		t.Fatalf("ListObjects page1: %v", err)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true for page 1")
	}
	if len(result.Objects) != 2 {
		t.Errorf("page 1 got %d objects, want 2", len(result.Objects))
	}

	result2, err := testStore.ListObjects(ctx, prefix, result.NextContinuationToken, 2)
	if err != nil {
		t.Fatalf("ListObjects page2: %v", err)
	}
	if result2.IsTruncated {
		t.Error("expected IsTruncated=false for page 2")
	}
	if len(result2.Objects) != 1 {
		t.Errorf("page 2 got %d objects, want 1", len(result2.Objects))
	}
}

// TestStore_GetBackendWithSpace_RespectsOrder is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_GetBackendWithSpace_RespectsOrder(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	name, err := testStore.GetBackendWithSpace(ctx, 10, []string{"minio-1", "minio-2"})
	if err != nil {
		t.Fatalf("GetBackendWithSpace: %v", err)
	}
	if name != "minio-1" {
		t.Errorf("got %q, want %q (first in order)", name, "minio-1")
	}

	name, err = testStore.GetBackendWithSpace(ctx, 10, []string{"minio-2", "minio-1"})
	if err != nil {
		t.Fatalf("GetBackendWithSpace reversed: %v", err)
	}
	if name != "minio-2" {
		t.Errorf("got %q, want %q (first in reversed order)", name, "minio-2")
	}

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "fill"), Backend: "minio-1", Size: 1024}); err != nil {
		t.Fatalf("RecordObject fill: %v", err)
	}
	name, err = testStore.GetBackendWithSpace(ctx, 1, []string{"minio-1", "minio-2"})
	if err != nil {
		t.Fatalf("GetBackendWithSpace after fill: %v", err)
	}
	if name != "minio-2" {
		t.Errorf("got %q, want %q (minio-1 full)", name, "minio-2")
	}
}

// TestStore_GetLeastUtilizedBackend_PicksLeastFull is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_GetLeastUtilizedBackend_PicksLeastFull(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	name, err := testStore.GetLeastUtilizedBackend(ctx, 10, []string{"minio-1", "minio-2"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend empty: %v", err)
	}
	if name != "minio-1" && name != "minio-2" {
		t.Errorf("unexpected backend %q", name)
	}

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "fill"), Backend: "minio-1", Size: 500}); err != nil {
		t.Fatalf("RecordObject fill: %v", err)
	}
	name, err = testStore.GetLeastUtilizedBackend(ctx, 10, []string{"minio-1", "minio-2"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend after fill: %v", err)
	}
	if name != "minio-2" {
		t.Errorf("got %q, want %q (minio-2 is least utilized)", name, "minio-2")
	}
}

// TestStore_GetLeastUtilizedBackend_RespectsMinSize is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_GetLeastUtilizedBackend_RespectsMinSize(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: uniqueKey(t, "full"), Backend: "minio-1", Size: 1024}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	name, err := testStore.GetLeastUtilizedBackend(ctx, 1, []string{"minio-1", "minio-2"})
	if err != nil {
		t.Fatalf("GetLeastUtilizedBackend: %v", err)
	}
	if name != "minio-2" {
		t.Errorf("got %q, want %q (minio-1 full)", name, "minio-2")
	}
}

// TestStore_RecordReplica_StaleSourceSkipped is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_RecordReplica_StaleSourceSkipped(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	key := uniqueKey(t, "store-replica")

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	_, ok, err := testStore.RecordReplica(ctx, key, "minio-2", "minio-1")
	if err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if !ok {
		t.Error("first RecordReplica = false, want true")
	}
	if used := queryQuotaUsed(t, "minio-2"); used != 100 {
		t.Errorf("minio-2 after replica = %d, want 100", used)
	}

	if _, err := testStore.DeleteObject(ctx, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-2", Size: 50}); err != nil {
		t.Fatalf("RecordObject fresh: %v", err)
	}

	_, ok, err = testStore.RecordReplica(ctx, key, "minio-1", "minio-1")
	if err != nil {
		t.Fatalf("RecordReplica stale: %v", err)
	}
	if ok {
		t.Error("stale RecordReplica = true, want false (source doesn't exist)")
	}
}

// TestStore_RecordPart_InvalidPartNumber is one of the sub-cases extracted from the
// original mega-TestStore; behaviour is preserved.
func TestStore_RecordPart_InvalidPartNumber(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	resetState(t)

	for _, pn := range []int{0, -1, 10001, 1 << 20} {
		err := testStore.RecordPart(ctx, "upload-invalid", pn, "\"etag\"", 100, nil)
		if err == nil {
			t.Errorf("RecordPart(%d) should fail, got nil", pn)
		}
	}
}

// -------------------------------------------------------------------------
// SYNC PIPELINE
// -------------------------------------------------------------------------

// TestSyncPipeline_ImportAndVerify is one of the sub-cases extracted from the
// original mega-TestSyncPipeline; behaviour is preserved.
func TestSyncPipeline_ImportAndVerify(t *testing.T) {
	ctx := context.Background()
	resetState(t)

	prefix := fmt.Sprintf("sync-test/%d/", time.Now().UnixNano())
	keys := seedDirectMinioPrefixed(t, ctx, prefix, 5, 80, "S")

	backend := newTestS3Backend(t, "minio-1")
	imported, skipped, err := runSyncPipeline(ctx, backend, internalKey(prefix))
	if err != nil {
		t.Fatalf("ListObjects+ImportObject pipeline: %v", err)
	}
	if imported != 5 {
		t.Errorf("imported = %d, want 5", imported)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if used := queryQuotaUsed(t, "minio-1"); used != 400 {
		t.Errorf("minio-1 bytes_used = %d, want 400", used)
	}
	assertProxyServesAll(t, ctx, newS3Client(t), keys, 80)
}

// seedDirectMinioPrefixed writes count objects directly to minio-1
// under the given prefix, each filled with sizeBytes copies of fillByte.
// Returns the user-visible keys.
func seedDirectMinioPrefixed(t *testing.T, ctx context.Context, prefix string, count int, sizeBytes int, fillByte string) []string {
	t.Helper()
	directClient := newDirectMinioClient(t, "minio-1")
	keys := make([]string, count)
	for i := range keys {
		keys[i] = fmt.Sprintf("%sobj-%d", prefix, i)
		_, err := directClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String("backend1"),
			Key:           aws.String(internalKey(keys[i])),
			Body:          bytes.NewReader(bytes.Repeat([]byte(fillByte), sizeBytes)),
			ContentLength: aws.Int64(int64(sizeBytes)),
		})
		if err != nil {
			t.Fatalf("direct PutObject(%s): %v", keys[i], err)
		}
	}
	return keys
}

// runSyncPipeline lists objects under prefix from backend and imports
// each into the metadata store on minio-1, returning the counts of
// freshly inserted (imported) and already-tracked (skipped) rows.
func runSyncPipeline(ctx context.Context, backend *s3be.S3Backend, prefix string) (int, int, error) {
	var imported, skipped int
	err := backend.ListObjects(ctx, prefix, func(objects []s3be.ListedObject) error {
		for _, obj := range objects {
			outcome, err := testStore.ImportObject(ctx, obj.Key, "minio-1", obj.SizeBytes, false, nil)
			if err != nil {
				return fmt.Errorf("ImportObject(%s): %w", obj.Key, err)
			}
			if outcome == core.ImportInserted {
				imported++
			} else {
				skipped++
			}
		}
		return nil
	})
	return imported, skipped, err
}

// TestSyncPipeline_IdempotentRerun is one of the sub-cases extracted from the
// original mega-TestSyncPipeline; behaviour is preserved.
func TestSyncPipeline_IdempotentRerun(t *testing.T) {
	ctx := context.Background()
	resetState(t)

	prefix := fmt.Sprintf("sync-idem/%d/", time.Now().UnixNano())
	_ = seedDirectMinioPrefixed(t, ctx, prefix, 3, 60, "I")

	backend := newTestS3Backend(t, "minio-1")

	imp1, skip1, err := runSyncPipeline(ctx, backend, internalKey(prefix))
	if err != nil {
		t.Fatalf("sync pipeline run 1: %v", err)
	}
	if imp1 != 3 || skip1 != 0 {
		t.Errorf("run 1: imported=%d skipped=%d, want 3/0", imp1, skip1)
	}

	imp2, skip2, err := runSyncPipeline(ctx, backend, internalKey(prefix))
	if err != nil {
		t.Fatalf("sync pipeline run 2: %v", err)
	}
	if imp2 != 0 || skip2 != 3 {
		t.Errorf("run 2: imported=%d skipped=%d, want 0/3", imp2, skip2)
	}

	if used := queryQuotaUsed(t, "minio-1"); used != 180 {
		t.Errorf("minio-1 bytes_used = %d, want 180 (not double-counted)", used)
	}
}

// -------------------------------------------------------------------------
// AUTH (SigV4)
// -------------------------------------------------------------------------

// TestAuthSigV4_ValidCredentials is one of the sub-cases extracted from the
// original mega-TestAuthSigV4; behaviour is preserved.
func TestAuthSigV4_ValidCredentials(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	const (
		authKey    = "TESTKEY0123456789"
		authSecret = "TESTSECRET0123456789abcdefghijklm"
	)
	authSrv := &s3api.Server{
		Objects:   testManager.Objects(),
		Multipart: testManager.Multipart(),
	}
	_ = authSrv
	authSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{
					AccessKeyID:     authKey,
					SecretAccessKey: authSecret,
				},
			},
		},
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authAddr := listener.Addr().String()
	_ = authAddr
	httpServer := &http.Server{
		Handler:      authSrv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	_ = httpServer
	go httpServer.Serve(listener)
	defer httpServer.Shutdown(ctx)
	authClient := func(key, secret string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + authAddr),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
			UsePathStyle: true,
		})
	}
	_ = authClient

	client := authClient(authKey, authSecret)
	key := uniqueKey(t, "auth")
	body := []byte("authenticated-content")

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject with valid creds: %v", err)
	}

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject with valid creds: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// TestAuthSigV4_WrongCredentials403 is one of the sub-cases extracted from the
// original mega-TestAuthSigV4; behaviour is preserved.
func TestAuthSigV4_WrongCredentials403(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	const (
		authKey    = "TESTKEY0123456789"
		authSecret = "TESTSECRET0123456789abcdefghijklm"
	)
	authSrv := &s3api.Server{
		Objects:   testManager.Objects(),
		Multipart: testManager.Multipart(),
	}
	_ = authSrv
	authSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{
					AccessKeyID:     authKey,
					SecretAccessKey: authSecret,
				},
			},
		},
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authAddr := listener.Addr().String()
	_ = authAddr
	httpServer := &http.Server{
		Handler:      authSrv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	_ = httpServer
	go httpServer.Serve(listener)
	defer httpServer.Shutdown(ctx)
	authClient := func(key, secret string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + authAddr),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
			UsePathStyle: true,
		})
	}
	_ = authClient

	client := authClient("WRONGKEY", "WRONGSECRET")
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("any-key"),
	})
	if err == nil {
		t.Fatal("expected error with wrong credentials, got nil")
	}
	assertHTTPStatus(t, err, 403)
}

// TestAuthSigV4_UnsignedRequest403 is one of the sub-cases extracted from the
// original mega-TestAuthSigV4; behaviour is preserved.
func TestAuthSigV4_UnsignedRequest403(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	const (
		authKey    = "TESTKEY0123456789"
		authSecret = "TESTSECRET0123456789abcdefghijklm"
	)
	authSrv := &s3api.Server{
		Objects:   testManager.Objects(),
		Multipart: testManager.Multipart(),
	}
	_ = authSrv
	authSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{
					AccessKeyID:     authKey,
					SecretAccessKey: authSecret,
				},
			},
		},
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authAddr := listener.Addr().String()
	_ = authAddr
	httpServer := &http.Server{
		Handler:      authSrv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	_ = httpServer
	go httpServer.Serve(listener)
	defer httpServer.Shutdown(ctx)
	authClient := func(key, secret string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + authAddr),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
			UsePathStyle: true,
		})
	}
	_ = authClient

	url := fmt.Sprintf("http://%s/%s/any-key", authAddr, virtualBucket)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestAuthSigV4_SpecialCharKeysSigV4 is one of the sub-cases extracted from the
// original mega-TestAuthSigV4; behaviour is preserved.
func TestAuthSigV4_SpecialCharKeysSigV4(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	const (
		authKey    = "TESTKEY0123456789"
		authSecret = "TESTSECRET0123456789abcdefghijklm"
	)
	authSrv := &s3api.Server{
		Objects:   testManager.Objects(),
		Multipart: testManager.Multipart(),
	}
	_ = authSrv
	authSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{
					AccessKeyID:     authKey,
					SecretAccessKey: authSecret,
				},
			},
		},
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authAddr := listener.Addr().String()
	_ = authAddr
	httpServer := &http.Server{
		Handler:      authSrv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	_ = httpServer
	go httpServer.Serve(listener)
	defer httpServer.Shutdown(ctx)
	authClient := func(key, secret string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + authAddr),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
			UsePathStyle: true,
		})
	}
	_ = authClient

	client := authClient(authKey, authSecret)

	keys := []string{
		uniqueKey(t, "auth") + "/my file.txt",
		uniqueKey(t, "auth") + "/a+b.dat",
		uniqueKey(t, "auth") + "/path/with spaces/and+plus",
	}

	for _, key := range keys {
		body := []byte("content-for-" + key)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject key=%q: %v", key, err)
		}

		resp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject key=%q: %v", key, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if !bytes.Equal(got, body) {
			t.Errorf("key=%q: body mismatch: got %q, want %q", key, got, body)
		}
	}
}

// TestAuthSigV4_AccessDeniedDoesNotLeakBucketName is one of the sub-cases extracted from the
// original mega-TestAuthSigV4; behaviour is preserved.
func TestAuthSigV4_AccessDeniedDoesNotLeakBucketName(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	const (
		authKey    = "TESTKEY0123456789"
		authSecret = "TESTSECRET0123456789abcdefghijklm"
	)
	authSrv := &s3api.Server{
		Objects:   testManager.Objects(),
		Multipart: testManager.Multipart(),
	}
	_ = authSrv
	authSrv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{
					AccessKeyID:     authKey,
					SecretAccessKey: authSecret,
				},
			},
		},
	}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authAddr := listener.Addr().String()
	_ = authAddr
	httpServer := &http.Server{
		Handler:      authSrv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	_ = httpServer
	go httpServer.Serve(listener)
	defer httpServer.Shutdown(ctx)
	authClient := func(key, secret string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + authAddr),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
			UsePathStyle: true,
		})
	}
	_ = authClient

	url := fmt.Sprintf("http://%s/%s/any-key", authAddr, virtualBucket)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), virtualBucket) {
		t.Errorf("403 response body should not contain bucket name %q, got: %s", virtualBucket, body)
	}
}

// -------------------------------------------------------------------------
// CIRCUIT BREAKER DEGRADED MODE
// -------------------------------------------------------------------------

// TestCircuitBreakerDegradedMode_ReadsDuringOutage is one of the sub-cases extracted from the
// original mega-TestCircuitBreakerDegradedMode; behaviour is preserved.
func TestCircuitBreakerDegradedMode_ReadsDuringOutage(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "cb-read")
	body := bytes.Repeat([]byte("R"), 100)
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	testFailableStore.SetFailing(true)
	defer testFailableStore.SetFailing(false)

	tripCircuitBreaker(t)

	if testDatabaseCB.IsHealthy() {
		t.Fatal("expected circuit to be open")
	}

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject during outage should succeed via broadcast: %v", err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}

	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject during outage should succeed via broadcast: %v", err)
	}

	testFailableStore.SetFailing(false)
	waitForRecovery(t)

	if !testDatabaseCB.IsHealthy() {
		t.Error("expected circuit to be closed after recovery")
	}

	resp2, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after recovery: %v", err)
	}
	resp2.Body.Close()
}

// TestCircuitBreakerDegradedMode_WritesRejectedDuringOutage is one of the sub-cases extracted from the
// original mega-TestCircuitBreakerDegradedMode; behaviour is preserved.
func TestCircuitBreakerDegradedMode_WritesRejectedDuringOutage(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	testFailableStore.SetFailing(true)
	defer testFailableStore.SetFailing(false)

	tripCircuitBreaker(t)

	if testDatabaseCB.IsHealthy() {
		t.Fatal("expected circuit to be open")
	}

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "cb-write")),
		Body:          bytes.NewReader([]byte("x")),
		ContentLength: aws.Int64(1),
	})
	if err == nil {
		t.Fatal("PutObject should fail during outage")
	}
	assertHTTPStatus(t, err, 503)

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String("any-key"),
	})
	if err == nil {
		t.Fatal("DeleteObject should fail during outage")
	}
	assertHTTPStatus(t, err, 503)

	_, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(virtualBucket),
	})
	if err == nil {
		t.Fatal("ListObjectsV2 should fail during outage")
	}
	assertHTTPStatus(t, err, 503)

	testFailableStore.SetFailing(false)
	waitForRecovery(t)

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(uniqueKey(t, "cb-write-after")),
		Body:          bytes.NewReader([]byte("y")),
		ContentLength: aws.Int64(1),
	})
	if err != nil {
		t.Fatalf("PutObject after recovery should succeed: %v", err)
	}
}

// -------------------------------------------------------------------------
// BUCKET OPERATIONS
// -------------------------------------------------------------------------

// TestBucketOperations_HeadBucket is one of the sub-cases extracted from the
// original mega-TestBucketOperations; behaviour is preserved.
func TestBucketOperations_HeadBucket(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(virtualBucket),
	})
	if err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
}

// TestBucketOperations_GetBucketLocation is one of the sub-cases extracted from the
// original mega-TestBucketOperations; behaviour is preserved.
func TestBucketOperations_GetBucketLocation(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resp, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: aws.String(virtualBucket),
	})
	if err != nil {
		t.Fatalf("GetBucketLocation: %v", err)
	}

	if resp.LocationConstraint != "" {
		t.Errorf("LocationConstraint = %q, want empty", resp.LocationConstraint)
	}
}

// TestBucketOperations_ListBuckets is one of the sub-cases extracted from the
// original mega-TestBucketOperations; behaviour is preserved.
func TestBucketOperations_ListBuckets(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx

	resp, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(resp.Buckets) == 0 {
		t.Fatal("expected at least one bucket")
	}
	found := false
	for _, b := range resp.Buckets {
		if aws.ToString(b.Name) == virtualBucket {
			found = true
		}
	}
	if !found {
		t.Errorf("ListBuckets did not include %q", virtualBucket)
	}
}

// -------------------------------------------------------------------------
// LIST OBJECTS V1
// -------------------------------------------------------------------------

// TestListObjectsV1_BasicList is one of the sub-cases extracted from the
// original mega-TestListObjectsV1; behaviour is preserved.
func TestListObjectsV1_BasicList(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx
	prefix := fmt.Sprintf("listv1/%d/", time.Now().UnixNano())
	_ = prefix
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	_ = keys
	for _, k := range keys {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader([]byte("data")),
			ContentLength: aws.Int64(4),
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}

	resp, err := client.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(resp.Contents) != 3 {
		t.Errorf("got %d objects, want 3", len(resp.Contents))
	}
}

// TestListObjectsV1_WithMarker is one of the sub-cases extracted from the
// original mega-TestListObjectsV1; behaviour is preserved.
func TestListObjectsV1_WithMarker(t *testing.T) {
	client := newS3Client(t)
	_ = client
	ctx := context.Background()
	_ = ctx
	prefix := fmt.Sprintf("listv1/%d/", time.Now().UnixNano())
	_ = prefix
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	_ = keys
	for _, k := range keys {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader([]byte("data")),
			ContentLength: aws.Int64(4),
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}

	resp, err := client.ListObjects(ctx, &s3.ListObjectsInput{
		Bucket: aws.String(virtualBucket),
		Prefix: aws.String(prefix),
		Marker: aws.String(keys[0]),
	})
	if err != nil {
		t.Fatalf("ListObjects with marker: %v", err)
	}
	if len(resp.Contents) != 2 {
		t.Fatalf("got %d objects, want 2", len(resp.Contents))
	}

	for _, obj := range resp.Contents {
		k := aws.ToString(obj.Key)
		if k == keys[0] {
			t.Errorf("marker key %q should not appear in results", k)
		}
	}
}

// -------------------------------------------------------------------------
// DELETE OBJECTS (BATCH)
// -------------------------------------------------------------------------

// TestDeleteObjects verifies the delete objects contract.
// Asserts that PutObject():.
func TestDeleteObjects(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	keys := make([]string, 3)
	for i := range keys {
		keys[i] = uniqueKey(t, "batch-del")
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader([]byte("data")),
			ContentLength: aws.Int64(4),
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", keys[i], err)
		}
	}

	deleteObjects := make([]types.ObjectIdentifier, len(keys))
	for i, k := range keys {
		deleteObjects[i] = types.ObjectIdentifier{Key: aws.String(k)}
	}

	resp, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(virtualBucket),
		Delete: &types.Delete{
			Objects: deleteObjects,
			Quiet:   aws.Bool(false),
		},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(resp.Deleted) != 3 {
		t.Errorf("deleted %d objects, want 3", len(resp.Deleted))
	}
	if len(resp.Errors) != 0 {
		t.Errorf("got %d errors, want 0", len(resp.Errors))
	}

	// Verify all are gone
	for _, k := range keys {
		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(k),
		})
		if err == nil {
			t.Errorf("expected 404 for %q after batch delete", k)
		}
	}
}

// -------------------------------------------------------------------------
// LIST MULTIPART UPLOADS
// -------------------------------------------------------------------------

// TestListMultipartUploads verifies the list multipart uploads contract.
// Asserts that CreateMultipartUpload:.
func TestListMultipartUploads(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "list-mpu")

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := create.UploadId

	list, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(virtualBucket),
	})
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}

	found := false
	for _, u := range list.Uploads {
		if aws.ToString(u.UploadId) == aws.ToString(uploadID) {
			found = true
			if aws.ToString(u.Key) != key {
				t.Errorf("upload key = %q, want %q", aws.ToString(u.Key), key)
			}
		}
	}
	if !found {
		t.Errorf("ListMultipartUploads did not include upload %s", aws.ToString(uploadID))
	}

	// Clean up
	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	})
	if err != nil {
		t.Fatalf("AbortMultipartUpload cleanup: %v", err)
	}
}

// -------------------------------------------------------------------------
// ABORT MULTIPART UPLOAD
// -------------------------------------------------------------------------

// TestAbortMultipartUpload verifies the abort multipart upload contract.
// Asserts that CreateMultipartUpload:.
func TestAbortMultipartUpload(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "abort-mpu")

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := create.UploadId

	_, err = client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		UploadId:      uploadID,
		PartNumber:    aws.Int32(1),
		Body:          bytes.NewReader(bytes.Repeat([]byte("A"), 100)),
		ContentLength: aws.Int64(100),
	})
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	})
	if err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}

	// Verify the upload is gone: ListParts on an aborted upload must
	// return NoSuchUpload (matching real S3 semantics), not silently
	// fall through to an empty parts list.
	_, err = client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	})
	if err == nil {
		t.Fatal("ListParts after abort: expected NoSuchUpload error, got nil")
	}
	if !strings.Contains(err.Error(), "NoSuchUpload") {
		t.Errorf("ListParts after abort: error = %v, want NoSuchUpload", err)
	}
}

// -------------------------------------------------------------------------
// LIST PARTS
// -------------------------------------------------------------------------

// TestListParts verifies the list parts contract.
// Asserts that CreateMultipartUpload:.
func TestListParts(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "list-parts")

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := create.UploadId

	for i := int32(1); i <= 2; i++ {
		_, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(i),
			Body:          bytes.NewReader(bytes.Repeat([]byte{byte(i)}, 100)),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i, err)
		}
	}

	resp, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}

	if len(resp.Parts) != 2 {
		t.Errorf("got %d parts, want 2", len(resp.Parts))
	}
	for i, p := range resp.Parts {
		wantNum := int32(i + 1)
		if aws.ToInt32(p.PartNumber) != wantNum {
			t.Errorf("part[%d].PartNumber = %d, want %d", i, aws.ToInt32(p.PartNumber), wantNum)
		}
		if aws.ToInt64(p.Size) != 100 {
			t.Errorf("part[%d].Size = %d, want 100", i, aws.ToInt64(p.Size))
		}
	}

	// Clean up
	_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	})
}

// -------------------------------------------------------------------------
// DRAIN & REMOVE
// -------------------------------------------------------------------------

// TestDrainBackend verifies the drain backend contract.
// Asserts that PutObject[]:.
func TestDrainBackend(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	ws := newWriteSet(t, client)
	keys := ws.seed(ctx, "drain", 5, 50)
	assertObjectsOnBackend(t, keys, "minio-1")
	ws.assertIntact(ctx, "before drain")

	if err := testManager.Drain().StartDrain(ctx, "minio-1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	waitDrainComplete(t, ctx, "minio-1", 30*time.Second)

	assertObjectsOnBackend(t, keys, "minio-2")
	ws.assertIntact(ctx, "after drain")
	assertNoLocationsOnBackend(t, "minio-1")
}

// assertObjectsOnBackend asserts every key resolves to wantBackend in
// object_locations.
func assertObjectsOnBackend(t *testing.T, keys []string, wantBackend string) {
	t.Helper()
	for _, key := range keys {
		if b := queryObjectBackend(t, key); b != wantBackend {
			t.Errorf("object %s on %s, want %s", key, b, wantBackend)
		}
	}
}

// waitDrainComplete polls GetDrainProgress until it reports inactive
// or the deadline elapses. Surfaces a stored error string if the drain
// finished unsuccessfully.
func waitDrainComplete(t *testing.T, ctx context.Context, backend string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatalf("drain of %s did not complete within %s", backend, timeout)
		default:
		}
		progress, err := testManager.Drain().GetDrainProgress(ctx, backend)
		if err != nil {
			t.Fatalf("GetDrainProgress: %v", err)
		}
		if !progress.Active {
			if progress.Error != "" {
				t.Fatalf("drain failed: %s", progress.Error)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertNoLocationsOnBackend asserts no object_locations rows reference
// backend, used to confirm a drain emptied the source completely.
func assertNoLocationsOnBackend(t *testing.T, backend string) {
	t.Helper()
	var count int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM object_locations WHERE backend_name = $1", backend).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("%s still has %d object_locations rows after drain", backend, count)
	}
}

// TestDrainBackend_WriteExclusion verifies the drain backend write exclusion contract.
// Asserts that PutObject seed:.
func TestDrainBackend_WriteExclusion(t *testing.T) {
	resetState(t)
	client := newS3Client(t)
	ctx := context.Background()

	// Put one object so the drain has something to work on.
	seedKey := uniqueKey(t, "drain-excl-seed")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(seedKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("S"), 50)),
		ContentLength: aws.Int64(50),
	})
	if err != nil {
		t.Fatalf("PutObject seed: %v", err)
	}

	// Start drain of minio-1.
	if err := testManager.Drain().StartDrain(ctx, "minio-1"); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}

	// While draining, new writes should go to minio-2 only.
	newKey := uniqueKey(t, "drain-excl-new")
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(newKey),
		Body:          bytes.NewReader(bytes.Repeat([]byte("N"), 50)),
		ContentLength: aws.Int64(50),
	})
	if err != nil {
		t.Fatalf("PutObject during drain: %v", err)
	}

	if b := queryObjectBackend(t, newKey); b != "minio-2" {
		t.Errorf("new object during drain on %s, want minio-2", b)
	}

	// Wait for drain to finish.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("drain did not complete within 30s")
		default:
		}
		progress, err := testManager.Drain().GetDrainProgress(ctx, "minio-1")
		if err != nil {
			t.Fatalf("GetDrainProgress: %v", err)
		}
		if !progress.Active {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRemoveBackend verifies the remove backend contract.
// Asserts that RecordObject:.
func TestRemoveBackend(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	// Directly record objects on minio-2 via the store so we can test
	// remove without affecting minio-1 (which other tests depend on).
	for i := range 3 {
		key := fmt.Sprintf("%s/remove-test-%d-%d", virtualBucket, i, time.Now().UnixNano())
		if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-2", Size: 100}); err != nil {
			t.Fatalf("RecordObject: %v", err)
		}
	}

	// Verify records exist.
	var before int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM object_locations WHERE backend_name = 'minio-2'").Scan(&before); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if before < 3 {
		t.Fatalf("expected >= 3 object_locations for minio-2, got %d", before)
	}

	// Remove without purge (just DB records).
	if err := testManager.Drain().RemoveBackend(ctx, "minio-2", false, nil); err != nil {
		t.Fatalf("RemoveBackend: %v", err)
	}

	// object_locations should be gone.
	var after int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM object_locations WHERE backend_name = 'minio-2'").Scan(&after); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if after != 0 {
		t.Errorf("minio-2 still has %d object_locations after remove", after)
	}

	// Quota row should be gone.
	var quotaCount int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM backend_quotas WHERE backend_name = 'minio-2'").Scan(&quotaCount); err != nil {
		t.Fatalf("quota count query: %v", err)
	}
	if quotaCount != 0 {
		t.Errorf("minio-2 still has %d backend_quotas rows after remove", quotaCount)
	}

	// Re-sync quota so other tests aren't broken.
	if err := testStore.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "minio-1", QuotaBytes: 1024},
		{Name: "minio-2", QuotaBytes: 2048},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
}
