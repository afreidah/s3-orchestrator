// -------------------------------------------------------------------------------
// Integration Tests - ListParts Paging
//
// Author: Alex Freidah
//
// Pages through an upload of more than one ListParts page with the AWS SDK's
// paginator, which is what a client resuming a large upload does.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestListParts_SDKPaginatorSeesEveryPartOnce uploads 1001 one-byte parts and
// pages through them 400 at a time. Every part must come back exactly once, in
// order, across pages that report truncation until the last.
func TestListParts_SDKPaginatorSeesEveryPartOnce(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	key := uniqueKey(t, "list-parts-paging")
	const partCount = 1001

	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := created.UploadId
	t.Cleanup(func() {
		_, _ = client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(virtualBucket), Key: aws.String(key), UploadId: uploadID,
		})
	})

	uploadOneByteParts(t, client, key, uploadID, partCount)
	seen, pages := pageAllParts(t, client, key, uploadID, 400)

	if pages != 3 {
		t.Errorf("pages = %d, want 3 (400 + 400 + 201)", pages)
	}
	if len(seen) != partCount {
		t.Fatalf("saw %d parts, want %d", len(seen), partCount)
	}
	for i, n := range seen {
		if n != int32(i+1) {
			t.Fatalf("part at position %d is %d, want %d", i, n, i+1)
		}
	}
}

// uploadOneByteParts uploads parts 1 through count, one byte each.
func uploadOneByteParts(t *testing.T, client *s3.Client, key string, uploadID *string, count int32) {
	t.Helper()
	for n := int32(1); n <= count; n++ {
		if _, err := client.UploadPart(context.Background(), &s3.UploadPartInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(n),
			Body:          bytes.NewReader([]byte("x")),
			ContentLength: aws.Int64(1),
		}); err != nil {
			t.Fatalf("UploadPart(%d): %v", n, err)
		}
	}
}

// pageAllParts lists the upload's parts through the SDK paginator, pageSize at
// a time, and returns the part numbers in the order received and the number
// of pages it took. More than 10 pages means the paging never ends.
func pageAllParts(t *testing.T, client *s3.Client, key string, uploadID *string, pageSize int32) ([]int32, int) {
	t.Helper()
	pager := s3.NewListPartsPaginator(client, &s3.ListPartsInput{
		Bucket:   aws.String(virtualBucket),
		Key:      aws.String(key),
		UploadId: uploadID,
		MaxParts: aws.Int32(pageSize),
	})
	var seen []int32
	pages := 0
	for pager.HasMorePages() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatalf("ListParts page %d: %v", pages+1, err)
		}
		pages++
		if pages > 10 {
			t.Fatal("paginator did not stop after 10 pages")
		}
		for _, p := range page.Parts {
			seen = append(seen, aws.ToInt32(p.PartNumber))
		}
	}
	return seen, pages
}
