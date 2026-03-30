// -------------------------------------------------------------------------------
// Integration Tests - Presigned URL Support
//
// Author: Alex Freidah
//
// End-to-end tests for presigned URL authentication using the real AWS SDK v2
// presign client. Verifies that presigned GET and PUT URLs work through the
// full proxy stack, and that expired presigned URLs are correctly rejected.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// newPresignClient creates an AWS SDK v2 presign client pointed at the
// in-process proxy, using the same credentials as the regular test client.
func newPresignClient(t *testing.T) *s3.PresignClient {
	t.Helper()
	return s3.NewPresignClient(newS3Client(t))
}

// TestE2E_PresignedGetObject uploads an object normally, generates a presigned
// GET URL via the AWS SDK, fetches it with a plain HTTP client (no SigV4
// signing), and verifies the response body matches.
func TestE2E_PresignedGetObject(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	client := newS3Client(t)

	key := uniqueKey(t, "presigned-get")
	body := []byte("presigned download test payload")

	// Upload normally
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Generate presigned GET URL (5 minute expiry)
	presigner := newPresignClient(t)
	presigned, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}

	// Fetch with plain HTTP client — no AWS credentials
	resp, err := http.Get(presigned.URL)
	if err != nil {
		t.Fatalf("HTTP GET presigned URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned GET returned %d: %s", resp.StatusCode, respBody)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// TestE2E_PresignedPutObject generates a presigned PUT URL via the AWS SDK,
// uploads an object with a plain HTTP client, then verifies the object is
// retrievable through a normal authenticated GET.
func TestE2E_PresignedPutObject(t *testing.T) {
	resetState(t)
	ctx := context.Background()

	key := uniqueKey(t, "presigned-put")
	body := []byte("presigned upload test payload")

	// Generate presigned PUT URL (5 minute expiry)
	presigner := newPresignClient(t)
	presigned, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(int64(len(body))),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}

	// Upload with plain HTTP client — no AWS credentials
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, presigned.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range presigned.SignedHeader {
		for _, vv := range v {
			req.Header.Set(k, vv)
		}
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("HTTP PUT presigned URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned PUT returned %d: %s", resp.StatusCode, respBody)
	}

	// Verify the object is stored correctly via normal GET
	client := newS3Client(t)
	getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after presigned PUT: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()

	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch after presigned PUT: got %d bytes, want %d", len(got), len(body))
	}
}

// TestE2E_PresignedExpiredURL generates a presigned GET URL with a 1-second
// expiry, waits for it to expire, then verifies that the request is rejected
// with a 403 Forbidden.
func TestE2E_PresignedExpiredURL(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	client := newS3Client(t)

	key := uniqueKey(t, "presigned-expired")
	body := []byte("this URL will expire")

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Generate presigned URL with 1-second expiry
	presigner := newPresignClient(t)
	presigned, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(1*time.Second))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}

	// Wait for expiry
	time.Sleep(2 * time.Second)

	// Attempt to fetch — should be rejected
	resp, err := http.Get(presigned.URL)
	if err != nil {
		t.Fatalf("HTTP GET expired presigned URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expired presigned URL returned %d, want 403", resp.StatusCode)
	}
}
