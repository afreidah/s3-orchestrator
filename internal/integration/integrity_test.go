// -------------------------------------------------------------------------------
// Integration Tests - Data Integrity
//
// Author: Alex Freidah
//
// Assertions and tests for the property the rest of the suite mostly assumes:
// that object bytes survive the operations the orchestrator performs on them.
// Placement decisions are verified elsewhere by counting rows and comparing
// utilisation; these read the bytes back.
//
// Runs against real MinIO and PostgreSQL containers.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// replicationFactorTwo is the replication config these tests use to put a
// second copy of an object on another backend.
func replicationFactorTwo() config.ReplicationConfig {
	return config.ReplicationConfig{
		Factor:         2,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
}

// queryHashedCopies counts the copies of key that carry a stored content hash,
// which is what a later scrub compares the backend bytes against.
func queryHashedCopies(t *testing.T, key string) int {
	t.Helper()
	var n int
	err := testDB.QueryRow(
		`SELECT count(*) FROM object_locations
		 WHERE object_key = $1 AND content_hash IS NOT NULL AND content_hash <> ''`,
		internalKey(key),
	).Scan(&n)
	if err != nil {
		t.Fatalf("queryHashedCopies(%q): %v", key, err)
	}
	return n
}

// -------------------------------------------------------------------------
// ASSERTIONS
// -------------------------------------------------------------------------

// assertAllCopiesIntact reads every copy the ledger claims for key directly
// from its backend and requires each to hold exactly want.
//
// Reading each copy directly is the point. A GET through the proxy fails over
// to a healthy replica, so a single corrupted copy is invisible from the
// client side - which is exactly the state the integrity machinery exists to
// find. stage names the operation under test so a failure says which step lost
// the bytes.
func assertAllCopiesIntact(t *testing.T, ctx context.Context, key string, want []byte, stage string) {
	t.Helper()

	backends := queryObjectBackends(t, key)
	if len(backends) == 0 {
		t.Fatalf("%s: ledger lists no copies of %q", stage, key)
	}

	for _, name := range backends {
		be, ok := allBackends[name]
		if !ok {
			t.Fatalf("%s: ledger names backend %q, which is not configured", stage, name)
		}
		result, err := be.GetObject(ctx, internalKey(key), "")
		if err != nil {
			t.Fatalf("%s: copy on %s is unreadable: %v", stage, name, err)
		}
		got, readErr := io.ReadAll(result.Body)
		_ = result.Body.Close()
		if readErr != nil {
			t.Fatalf("%s: reading copy on %s: %v", stage, name, readErr)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: copy on %s holds %d bytes, want %d (contents differ)",
				stage, name, len(got), len(want))
		}
	}
}

// assertProxyServes requires a GET through the proxy to return exactly want.
func assertProxyServes(t *testing.T, ctx context.Context, client *s3.Client, key string, want []byte, stage string) {
	t.Helper()

	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("%s: GetObject(%q): %v", stage, key, err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: reading body of %q: %v", stage, key, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: proxy served %d bytes for %q, want %d (contents differ)",
			stage, len(got), key, len(want))
	}
}

// assertObjectIntact is the assertion to reach for after any operation that
// moves, copies, or removes object data: the client still sees the object, and
// every copy backing it holds the same bytes.
func assertObjectIntact(t *testing.T, ctx context.Context, client *s3.Client, key string, want []byte, stage string) {
	t.Helper()
	assertProxyServes(t, ctx, client, key, want, stage)
	assertAllCopiesIntact(t, ctx, key, want, stage)
}

// corruptBackendCopy overwrites an object's bytes on one backend, behind the
// orchestrator's back, standing in for bit rot or a bad write that the ledger
// knows nothing about.
func corruptBackendCopy(t *testing.T, ctx context.Context, backendName, key string, corrupt []byte) {
	t.Helper()

	be, ok := allBackends[backendName]
	if !ok {
		t.Fatalf("backend %q is not configured", backendName)
	}
	if _, err := be.PutObject(ctx, internalKey(key), bytes.NewReader(corrupt),
		int64(len(corrupt)), "application/octet-stream", nil); err != nil {
		t.Fatalf("corrupting copy on %s: %v", backendName, err)
	}
}

// -------------------------------------------------------------------------
// SCRUBBER
// -------------------------------------------------------------------------

// TestIntegrity_ScrubberDetectsCorruptedCopy is the end-to-end check the
// integrity machinery exists for: bytes on a backend change underneath the
// orchestrator, and the scrubber notices.
//
// The corruption is applied to one of two replicas, which is the case a client
// cannot see for itself - a GET fails over to the healthy copy and returns the
// right answer while the bad copy sits there indefinitely.
func TestIntegrity_ScrubberDetectsCorruptedCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "scrub-corrupt")
	body := bytes.Repeat([]byte("S"), 512)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Two copies, so the corrupted one has a healthy sibling to hide behind.
	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	backends := queryObjectBackends(t, key)
	if len(backends) != 2 {
		t.Fatalf("expected 2 copies before corrupting one, got %v", backends)
	}
	assertObjectIntact(t, ctx, client, key, body, "after replication")

	// Backfill records the hashes the scrub will later compare against. The
	// write path only stores them when integrity is configured, so this also
	// exercises the backfill path itself.
	sum, _ := testWorkers.Scrubber.Backfill(ctx, 100, 0, nil)
	if sum.Succeeded == 0 {
		t.Fatalf("backfill stored no hashes: %+v", sum)
	}
	if hashed := queryHashedCopies(t, key); hashed != 2 {
		t.Fatalf("expected 2 copies with a stored hash, got %d", hashed)
	}

	victim := backends[1]
	corruptBackendCopy(t, ctx, victim, key, bytes.Repeat([]byte("X"), 512))

	// The client still gets the right bytes: read failover picks the healthy
	// copy. This is precisely why corruption needs a background detector.
	assertProxyServes(t, ctx, client, key, body, "after corruption")

	scrubSum := testWorkers.Scrubber.Scrub(ctx, 100, nil)
	if scrubSum.Failed != 1 {
		t.Errorf("scrub reported %d mismatches, want 1 (%+v)", scrubSum.Failed, scrubSum)
	}

	// A detected mismatch removes the bad bytes: DeleteOrEnqueue deletes
	// directly and only falls back to the queue when the backend refuses.
	be := allBackends[victim]
	if _, err := be.GetObject(ctx, internalKey(key), ""); err == nil {
		t.Errorf("corrupted copy still present on %s after scrub", victim)
	}

	// The ledger must not keep pointing at bytes that were just deleted:
	// a surviving row overstates the replication factor and sends readers
	// to a copy that is now a 404.
	if remaining := queryObjectBackends(t, key); len(remaining) != 1 || remaining[0] == victim {
		t.Errorf("ledger still lists %v after the copy on %s was removed", remaining, victim)
	}

	// The healthy copy is untouched and still serves the object.
	assertObjectIntact(t, ctx, client, key, body, "after corrupt copy removed")
}

// TestIntegrity_ScrubberAcceptsHealthyCopies verifies a clean fleet produces no
// mismatches, so the detector above is not simply reporting everything.
func TestIntegrity_ScrubberAcceptsHealthyCopies(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	key := uniqueKey(t, "scrub-clean")
	body := bytes.Repeat([]byte("H"), 300)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if sum, _ := testWorkers.Scrubber.Backfill(ctx, 100, 0, nil); sum.Succeeded == 0 {
		t.Fatalf("backfill stored no hashes: %+v", sum)
	}

	scrubSum := testWorkers.Scrubber.Scrub(ctx, 100, nil)
	if scrubSum.Failed != 0 {
		t.Errorf("scrub reported %d mismatches on healthy copies, want 0 (%+v)",
			scrubSum.Failed, scrubSum)
	}
	assertObjectIntact(t, ctx, client, key, body, "after clean scrub")
}
