// -------------------------------------------------------------------------------
// Integration Tests - Encrypted Object Accounting
//
// Author: Alex Freidah
//
// Encryption grows an object by a fixed, computable amount: a 32-byte header
// plus 28 bytes per chunk. That is the difference between it and compression,
// whose output size is only known once the encoder has run. The ciphertext size
// of a write is knowable before a byte moves, so there is no reason for
// placement, admission or the usage counters to be working in plaintext units.
//
// These tests measure what a backend physically holds and require every ledger
// to agree with it. The counters are what the configured monthly limits are
// judged against, and a fleet charged in plaintext while billed in ciphertext
// overruns a budget it believes it is still under.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// putEncrypted writes body through the encryption-enabled proxy and returns the
// key, the backend it landed on, and the bytes that backend physically holds.
func putEncrypted(t *testing.T, env *encryptionTestEnv, prefix string, body []byte) (key, backendName string, physical int64) {
	t.Helper()
	key = uniqueKey(t, prefix)
	if _, err := env.proxyClient.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject through the encrypting proxy: %v", err)
	}
	backendName = queryObjectBackend(t, key)
	return key, backendName, backendObjectSize(t, backendName, key)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestUsage_EncryptedPutChargesCiphertext asserts a write is charged the
// envelope that landed, not the plaintext the client sent. The row already
// commits the ciphertext size, so charging the plaintext leaves the storage
// ledger and the bandwidth ledger describing the same object differently.
func TestUsage_EncryptedPutChargesCiphertext(t *testing.T) {
	env := setupEncryptionEnv(t)
	body := bytes.Repeat([]byte("E"), 512)

	var key, target string
	var physical int64
	deltas := fleetUsageDelta(env.stack.Runtime, testBackendOrder, func() {
		key, target, physical = putEncrypted(t, env, "usage-enc-put", body)
	})

	if want := env.encryptor.CiphertextSize(int64(len(body))); physical != want {
		t.Fatalf("backend holds %d bytes, want the ciphertext size %d", physical, want)
	}
	assertCharged(t, "encrypted PUT on "+target, deltas[target],
		usageSnapshot{APICalls: 1, Ingress: physical})
	if got := queryStoredSize(t, key); got != physical {
		t.Errorf("size_bytes = %d, want the %d bytes the backend holds", got, physical)
	}
}

// TestUsage_EncryptedGetChargesCiphertext asserts a read is charged the bytes
// that crossed the backend link. Decryption happens on this side of that link,
// so the plaintext the client receives is not what the backend served.
func TestUsage_EncryptedGetChargesCiphertext(t *testing.T) {
	env := setupEncryptionEnv(t)
	body := bytes.Repeat([]byte("G"), 512)
	key, target, physical := putEncrypted(t, env, "usage-enc-get", body)

	var got []byte
	delta := usageDelta(env.stack.Runtime, target, func() {
		out, err := env.proxyClient.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		defer out.Body.Close()
		got, err = io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
	})

	if !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, want the %d written", len(got), len(body))
	}
	assertCharged(t, "encrypted GET on "+target, delta,
		usageSnapshot{APICalls: 1, Egress: physical})
}

// TestUsage_EncryptedRangeGetChargesFetchedChunks is where the gap is widest.
// A range is served by fetching the whole chunks covering it, because a chunk
// is the unit AES-GCM can authenticate; the client's slice can be a handful of
// bytes out of the 64 KiB that actually left the backend. Charging the slice
// makes a range-heavy workload look nearly free while the provider bills it in
// full chunks.
func TestUsage_EncryptedRangeGetChargesFetchedChunks(t *testing.T) {
	env := setupEncryptionEnv(t)
	chunk := env.encryptor.ChunkSize()
	setQuotaLimits(t, 4<<20)

	body := bytes.Repeat([]byte("R"), chunk*3)
	key, target, _ := putEncrypted(t, env, "usage-enc-range", body)

	const want = 64
	var got []byte
	delta := usageDelta(env.stack.Runtime, target, func() {
		out, err := env.proxyClient.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
			Range:  aws.String("bytes=0-63"),
		})
		if err != nil {
			t.Fatalf("ranged GetObject: %v", err)
		}
		defer out.Body.Close()
		got, err = io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read ranged body: %v", err)
		}
	})

	if !bytes.Equal(got, body[:want]) {
		t.Fatalf("ranged read returned %d bytes that do not match the first %d written", len(got), want)
	}
	// One whole chunk plus its nonce and tag. The header is not refetched: the
	// base nonce needed to decrypt is already on the ledger row.
	fetched := int64(chunk + encryption.ChunkOverhead)
	assertCharged(t, "encrypted ranged GET on "+target, delta,
		usageSnapshot{APICalls: 1, Egress: fetched})
}

// TestUsage_EncryptedPlacementReservesCiphertextSize pins the storage side of
// the same mistake. Placement asks whether the object fits, and asking in
// plaintext units reserves less room than the commit will need.
//
// The quota itself is safe either way: IncrementQuota refuses a row that would
// carry a backend past its limit, so the write fails rather than overshooting.
// What the plaintext question costs is the trip - the envelope is uploaded to a
// backend that was never going to keep it, the commit is refused, and the bytes
// are deleted again. A write nothing can accept belongs refused before any
// backend is contacted, which is what leaves both of them untouched here.
//
// The limit is set one byte under the envelope, so the plaintext fits and the
// ciphertext does not.
func TestUsage_EncryptedPlacementReservesCiphertextSize(t *testing.T) {
	env := setupEncryptionEnv(t)
	body := bytes.Repeat([]byte("Q"), 512)
	limit := env.encryptor.CiphertextSize(int64(len(body))) - 1
	setQuotaLimits(t, limit)

	var err error
	deltas := fleetUsageDelta(env.stack.Runtime, testBackendOrder, func() {
		_, err = env.proxyClient.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(uniqueKey(t, "usage-enc-placement")),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
	})

	if err == nil {
		t.Errorf("PutObject succeeded: a %d byte object encrypts to %d, and no backend holds more than %d",
			len(body), limit+1, limit)
	}
	for _, name := range testBackendOrder {
		assertNothingCharged(t, "doomed encrypted PUT on "+name, deltas[name])
		if used := queryQuotaUsed(t, name); used != 0 {
			t.Errorf("%s: bytes_used = %d, want 0", name, used)
		}
	}
}

// TestUsage_EncryptedUploadPartChargesCiphertext asserts a part is charged what
// it occupies. Parts are encrypted individually under the upload's data key, so
// every part of a large upload carries its own envelope and the shortfall
// compounds across all of them.
func TestUsage_EncryptedUploadPartChargesCiphertext(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()
	part := bytes.Repeat([]byte("M"), 512)

	key := uniqueKey(t, "usage-enc-part")
	create, err := env.proxyClient.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)
	target := queryMultipartBackend(t, uploadID)

	delta := usageDelta(env.stack.Runtime, target, func() {
		if _, err := env.proxyClient.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(1),
			Body:          bytes.NewReader(part),
			ContentLength: aws.Int64(int64(len(part))),
		}); err != nil {
			t.Fatalf("UploadPart: %v", err)
		}
	})

	physical := backendRawObjectSize(t, target, multipartPartStoredKey(uploadID, 1))
	if want := env.encryptor.CiphertextSize(int64(len(part))); physical != want {
		t.Fatalf("part occupies %d bytes, want the ciphertext size %d", physical, want)
	}
	assertCharged(t, "encrypted UploadPart on "+target, delta,
		usageSnapshot{APICalls: 1, Ingress: physical})
}

// multipartPartStoredKey mirrors the temp key the multipart manager stores a
// part under. Duplicated rather than exported: the layout is internal to that
// package, and a test reaching for the real bytes has to name it somehow.
func multipartPartStoredKey(uploadID string, partNumber int) string {
	return fmt.Sprintf("__multipart/%s/%d", uploadID, partNumber)
}
