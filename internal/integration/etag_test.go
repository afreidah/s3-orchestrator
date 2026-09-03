//go:build integration

// -------------------------------------------------------------------------------
// Integration - ETag Identity
//
// Author: Alex Freidah
//
// End-to-end cover for the two properties a client relies on: the ETag is the
// MD5 of the bytes it uploaded, and it is the same value whichever copy of the
// object answers. The existing CRUD case only compares a PUT against a HEAD on
// the same backend, which passes even when every copy reports its own.
//
// Runs against a real Postgres and real backends so the value asserted is the
// one that survived the ledger round trip rather than one held in memory.
// -------------------------------------------------------------------------------

package integration

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: the S3 ETag algorithm, not a security control
	"encoding/hex"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// expectedETag renders the value S3 reports for a single-part upload of body.
func expectedETag(body []byte) string {
	sum := md5.Sum(body) //nolint:gosec // G401: see above
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestETag_IsMD5OfClientBytes pins the contract a client checks its own upload
// against: PUT and HEAD both report the digest of what it sent.
func TestETag_IsMD5OfClientBytes(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "etag")
	body := bytes.Repeat([]byte("etag-identity"), 32)
	want := expectedETag(body)

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
	if got := aws.ToString(putResp.ETag); got != want {
		t.Errorf("PUT etag = %q, want the MD5 of the uploaded bytes %q", got, want)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if got := aws.ToString(head.ETag); got != want {
		t.Errorf("HEAD etag = %q, want %q", got, want)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer get.Body.Close()
	if got := aws.ToString(get.ETag); got != want {
		t.Errorf("GET etag = %q, want %q", got, want)
	}
}

// TestETag_StoredOnEveryCopy is the failover property, asserted at the ledger
// rather than by killing a backend: every copy of the key has to carry the
// same recorded value, because that is what makes the answer independent of
// which one a read reaches.
func TestETag_StoredOnEveryCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()

	key := uniqueKey(t, "etag-copies")
	body := bytes.Repeat([]byte("replicated"), 64)
	want := expectedETag(body)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	rows, err := testDB.Query(
		"SELECT backend_name, COALESCE(etag, '') FROM object_locations WHERE object_key = $1",
		internalKey(key),
	)
	if err != nil {
		t.Fatalf("query etags: %v", err)
	}
	defer rows.Close()

	copies := 0
	for rows.Next() {
		var backendName, etag string
		if err := rows.Scan(&backendName, &etag); err != nil {
			t.Fatalf("scan etag row: %v", err)
		}
		copies++
		if etag != want {
			t.Errorf("copy on %s has etag %q, want %q", backendName, etag, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate etag rows: %v", err)
	}
	if copies == 0 {
		t.Fatal("no object_locations rows for the key")
	}
}
