//go:build integration

// -------------------------------------------------------------------------------
// Integration - Master Key Rotation Across Restarts
//
// Author: Alex Freidah
//
// Rotates the master key the way an operator does: objects written under key
// A, the orchestrator restarted with master_key B and A in previous_keys, an
// all-stale re-wrap through the admin API, and a final restart with A
// removed. Copies carry the positional key ID earlier releases recorded, so
// the rotation has to find the key that wrapped them without an ID match.
//
// Every step reads the objects back through the S3 API and compares the
// bytes, whole and by ranges that cross chunk boundaries, against what was
// written. Runs against a real Postgres and real backends.
// -------------------------------------------------------------------------------

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// legacyKeyID is the positional ID earlier releases recorded for an inline
// master key.
const legacyKeyID = "config-0"

// rotationObject is one object the rotation test wrote: its key, the bytes
// written, and the byte ranges read back at every step.
type rotationObject struct {
	key    string
	body   []byte
	ranges [][2]int
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestKeyRotation_LegacyKeyIDsSurviveRotation writes objects under key A,
// relabels their rows with the ID an earlier release recorded, and walks the
// rotation to key B across three orchestrator restarts. The objects must read
// back byte for byte at every step, the re-wrap must not touch the stored
// ciphertext, and a restart that drops A before the re-wrap must refuse to
// serve them.
func TestKeyRotation_LegacyKeyIDsSurviveRotation(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	raiseQuotasForChunks(t, ctx)

	keyA := bytes.Repeat([]byte("A"), config.KeySize)
	keyB := bytes.Repeat([]byte("B"), config.KeySize)
	b64A := base64.StdEncoding.EncodeToString(keyA)
	b64B := base64.StdEncoding.EncodeToString(keyB)

	objects := writeUnderKey(t, ctx, b64A, encryption.Fingerprint(keyA))
	for _, obj := range objects {
		setCopyKeyID(t, obj.key, legacyKeyID)
	}
	stored := make(map[string][]byte, len(objects))
	for _, obj := range objects {
		stored[obj.key] = readStoredBytes(t, ctx, obj.key)
	}

	onlyB := startEncryptionEnv(t, &config.EncryptionConfig{MasterKey: b64B})
	assertUnreadable(t, ctx, onlyB.proxyClient, objects)

	rotated := startEncryptionEnv(t, &config.EncryptionConfig{MasterKey: b64B, PreviousKeys: []string{b64A}})
	assertReadsBack(t, ctx, rotated.proxyClient, objects, "after restarting with previous_keys")

	res := rotated.callAdminWith(t, "/admin/api/rotate-encryption-key", adminapi.RotateEncryptionKeyRequest{AllStale: true})
	assertAdminCount(t, res, "rotated", len(objects))
	if failed := int(res["failed"].(float64)); failed != 0 {
		t.Errorf("rotation failed = %d, want 0", failed)
	}
	for _, obj := range objects {
		if got := queryCopyKeyID(t, obj.key); got != encryption.Fingerprint(keyB) {
			t.Errorf("%s key_id = %q after rotation, want %q", obj.key, got, encryption.Fingerprint(keyB))
		}
		if !bytes.Equal(readStoredBytes(t, ctx, obj.key), stored[obj.key]) {
			t.Errorf("%s stored ciphertext changed during a metadata-only rotation", obj.key)
		}
	}
	assertReadsBack(t, ctx, rotated.proxyClient, objects, "after the re-wrap")

	final := startEncryptionEnv(t, &config.EncryptionConfig{MasterKey: b64B})
	assertReadsBack(t, ctx, final.proxyClient, objects, "after dropping the old key")
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// writeUnderKey starts an orchestrator with only the master key b64 and writes
// a multi-chunk object ending in a partial chunk and a sub-chunk object
// through it, checking each copy records wantKeyID.
func writeUnderKey(t *testing.T, ctx context.Context, b64, wantKeyID string) []rotationObject {
	t.Helper()
	const large = encryptionChunkSize*3 + encryptionChunkSize/2
	objects := []rotationObject{
		{
			key:  uniqueKey(t, "rotate-large"),
			body: incompressible(large),
			ranges: [][2]int{
				{encryptionChunkSize - 6, 2*encryptionChunkSize + 9},
				{3*encryptionChunkSize - 8, large - 1},
			},
		},
		{
			key:    uniqueKey(t, "rotate-small"),
			body:   incompressible(1000),
			ranges: [][2]int{{10, 99}},
		},
	}

	env := startEncryptionEnv(t, &config.EncryptionConfig{MasterKey: b64})
	for _, obj := range objects {
		if _, err := env.proxyClient.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(obj.key),
			Body:          bytes.NewReader(obj.body),
			ContentLength: aws.Int64(int64(len(obj.body))),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", obj.key, err)
		}
		assertStoredAsEnvelope(t, ctx, obj.key, obj.body)
		if got := queryCopyKeyID(t, obj.key); got != wantKeyID {
			t.Fatalf("%s key_id = %q, want %q", obj.key, got, wantKeyID)
		}
	}
	return objects
}

// setCopyKeyID rewrites the key ID every copy of key records, standing in for
// rows an earlier release wrote.
func setCopyKeyID(t *testing.T, key, keyID string) {
	t.Helper()
	if _, err := testDB.Exec("UPDATE object_locations SET key_id = $1 WHERE object_key = $2", keyID, internalKey(key)); err != nil {
		t.Fatalf("set key_id for %s: %v", key, err)
	}
}

// queryCopyKeyID returns the key ID the copy of key records.
func queryCopyKeyID(t *testing.T, key string) string {
	t.Helper()
	var keyID string
	if err := testDB.QueryRow("SELECT key_id FROM object_locations WHERE object_key = $1", internalKey(key)).Scan(&keyID); err != nil {
		t.Fatalf("query key_id for %s: %v", key, err)
	}
	return keyID
}

// assertReadsBack reads every object whole and by each of its ranges through
// client, and requires the bytes to match what was written.
func assertReadsBack(t *testing.T, ctx context.Context, client *s3.Client, objects []rotationObject, phase string) {
	t.Helper()
	for _, obj := range objects {
		got, err := getObject(ctx, client, obj.key, "")
		if err != nil {
			t.Fatalf("%s: GET %s: %v", phase, obj.key, err)
		}
		if sha256.Sum256(got) != sha256.Sum256(obj.body) || !bytes.Equal(got, obj.body) {
			t.Fatalf("%s: GET %s returned %d bytes that differ from the %d written", phase, obj.key, len(got), len(obj.body))
		}
		for _, r := range obj.ranges {
			got, err := getObject(ctx, client, obj.key, fmt.Sprintf("bytes=%d-%d", r[0], r[1]))
			if err != nil {
				t.Fatalf("%s: GET %s range %v: %v", phase, obj.key, r, err)
			}
			if want := obj.body[r[0] : r[1]+1]; !bytes.Equal(got, want) {
				t.Fatalf("%s: GET %s range %v returned %d bytes that differ from the %d written", phase, obj.key, r, len(got), len(want))
			}
		}
	}
}

// assertUnreadable requires every GET of objects through client to fail
// rather than return any bytes.
func assertUnreadable(t *testing.T, ctx context.Context, client *s3.Client, objects []rotationObject) {
	t.Helper()
	for _, obj := range objects {
		if got, err := getObject(ctx, client, obj.key, ""); err == nil {
			t.Fatalf("GET %s without the key that wrapped it returned %d bytes, want an error", obj.key, len(got))
		}
	}
}

// getObject reads key through client, whole or by rangeHeader, without
// retries so a refused read fails at once. A body that breaks off midway
// returns its read error.
func getObject(ctx context.Context, client *s3.Client, key, rangeHeader string) ([]byte, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(virtualBucket), Key: aws.String(key)}
	if rangeHeader != "" {
		in.Range = aws.String(rangeHeader)
	}
	resp, err := client.GetObject(ctx, in, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}
