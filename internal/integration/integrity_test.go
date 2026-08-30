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
	"slices"
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

// enableVerifyOnRead turns on hash checking for the GET path and returns a
// function restoring the previous configuration. Verify-on-read is off by
// default, so a test that wants it has to ask.
func enableVerifyOnRead(t *testing.T) func() {
	t.Helper()

	previous := testStack.IntegrityCfg.Load()
	testStack.IntegrityCfg.Store(&config.IntegrityConfig{
		Enabled:      true,
		VerifyOnRead: true,
	})
	return func() { testStack.IntegrityCfg.Store(previous) }
}

// resyncQuotaLimits restores the configured quota limits, which backend
// removal deletes, so later tests still see the fleet they expect.
func resyncQuotaLimits(t *testing.T, ctx context.Context) {
	t.Helper()

	if err := testStore.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: "minio-1", QuotaBytes: 1024},
		{Name: "minio-2", QuotaBytes: 2048},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
}

// waitFor polls until cond returns true, failing the test if it never does.
// Detection that happens in the server's response-close path is not visible
// the instant the client's own Close returns.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
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

// -------------------------------------------------------------------------
// WRITE SET
// -------------------------------------------------------------------------

// writeSet writes objects through the proxy and remembers what it wrote, so a
// later assertion can require the same bytes back.
//
// The seeding loops elsewhere in this suite build a body, write it, and drop it
// on the floor, which leaves nothing to compare against once an operation has
// moved the object. Writing and recording happen in the same call here so the
// two cannot drift apart.
type writeSet struct {
	t      *testing.T
	client *s3.Client
	bodies map[string][]byte
	keys   []string
}

func newWriteSet(t *testing.T, client *s3.Client) *writeSet {
	t.Helper()
	return &writeSet{t: t, client: client, bodies: map[string][]byte{}}
}

// put writes body at key through the proxy and records it.
func (w *writeSet) put(ctx context.Context, key string, body []byte) {
	w.t.Helper()

	if _, err := w.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		w.t.Fatalf("writeSet.put(%q): %v", key, err)
	}

	recorded := bytes.Clone(body)
	if _, seen := w.bodies[key]; !seen {
		w.keys = append(w.keys, key)
	}
	w.bodies[key] = recorded
}

// seed writes count objects of size bytes each under prefix, giving every
// object a distinct body so a swap between two of them is detectable.
func (w *writeSet) seed(ctx context.Context, prefix string, count, size int) []string {
	w.t.Helper()

	keys := make([]string, count)
	for i := range keys {
		keys[i] = uniqueKey(w.t, prefix)
		body := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		w.put(ctx, keys[i], body)
	}
	return keys
}

// assertIntact requires every recorded object to still read back byte-for-byte,
// both through the proxy and from every copy the ledger claims for it.
func (w *writeSet) assertIntact(ctx context.Context, stage string) {
	w.t.Helper()

	if len(w.keys) == 0 {
		w.t.Fatalf("%s: write set is empty, nothing was verified", stage)
	}
	for _, key := range w.keys {
		assertObjectIntact(w.t, ctx, w.client, key, w.bodies[key], stage)
	}
}

// drop stops tracking key, for an object the test deleted on purpose.
func (w *writeSet) drop(key string) {
	delete(w.bodies, key)
	w.keys = slices.DeleteFunc(w.keys, func(k string) bool { return k == key })
}

// forget drops the recorded objects, for tests that call resetState partway
// through and start a second scenario against the same client.
func (w *writeSet) forget() {
	w.bodies = map[string][]byte{}
	w.keys = nil
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

// -------------------------------------------------------------------------
// VERIFY ON READ
// -------------------------------------------------------------------------

// TestIntegrity_VerifyOnReadDiscardsCorruptedCopy covers the second detector:
// the scrubber finds corruption on its own schedule, while verify-on-read
// finds it the moment a client happens to touch the bad copy.
//
// The object deliberately has a single copy. With a replica present the proxy
// fails over to the healthy one and the corrupted bytes are never read, so
// there would be nothing for this path to catch.
//
// Verification completes when the body is closed rather than mid-stream, so
// the client still receives the bad bytes for this request. What must happen
// is that the copy does not survive to serve a second one.
func TestIntegrity_VerifyOnReadDiscardsCorruptedCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	restore := enableVerifyOnRead(t)
	defer restore()

	key := uniqueKey(t, "verify-read")
	body := bytes.Repeat([]byte("V"), 400)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Integrity was enabled before the write, so the write path stored the
	// hash itself and there is nothing for a backfill to do.
	if hashed := queryHashedCopies(t, key); hashed != 1 {
		t.Fatalf("expected the written copy to carry a hash, got %d hashed copies", hashed)
	}

	backends := queryObjectBackends(t, key)
	if len(backends) != 1 {
		t.Fatalf("expected a single copy so the read cannot fail over, got %v", backends)
	}
	victim := backends[0]
	corruptBackendCopy(t, ctx, victim, key, bytes.Repeat([]byte("X"), 400))

	// Read the object to completion and close it, which is what drives
	// verification.
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	waitFor(t, 10*time.Second, "corrupted copy discarded", func() bool {
		be := allBackends[victim]
		_, getErr := be.GetObject(ctx, internalKey(key), "")
		return getErr != nil
	})

	// The ledger must not keep pointing at bytes that were just deleted. A
	// surviving row overstates the replication factor, so the replicator sees
	// the object as adequately covered and never rebuilds it.
	if remaining := queryObjectBackends(t, key); len(remaining) != 0 {
		t.Errorf("ledger still lists %v after the copy on %s was discarded", remaining, victim)
	}
}

// -------------------------------------------------------------------------
// BACKEND REMOVAL
// -------------------------------------------------------------------------

// TestIntegrity_RemoveBackendKeepsSurvivingCopies removes a backend out from
// under a replicated object and requires the remaining copy to still hold the
// original bytes. Removal rewrites ledger rows in bulk, which is exactly the
// kind of operation that can leave an object pointing at the wrong copy.
func TestIntegrity_RemoveBackendKeepsSurvivingCopies(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	ws := newWriteSet(t, client)
	keys := ws.seed(ctx, "remove-backend", 3, 120)

	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	for _, key := range keys {
		if copies := queryObjectBackends(t, key); len(copies) != 2 {
			t.Fatalf("expected 2 copies of %q before removal, got %v", key, copies)
		}
	}
	ws.assertIntact(ctx, "before backend removal")

	if err := testStack.Drain.RemoveBackend(ctx, "minio-2", false, nil); err != nil {
		t.Fatalf("RemoveBackend: %v", err)
	}
	defer resyncQuotaLimits(t, ctx)

	for _, key := range keys {
		if copies := queryObjectBackends(t, key); len(copies) != 1 {
			t.Errorf("expected 1 copy of %q after removal, got %v", key, copies)
		}
	}
	ws.assertIntact(ctx, "after backend removal")
}

// TestIntegrity_RangedReadKeepsHealthyCopy is the end-to-end guard for a
// data-loss bug: with verify-on-read enabled, a ranged GET of a healthy object
// used to destroy the copy it read. The stored hash covers the whole object, so
// the slice that was served could never match it, and the mismatch handler
// deleted both the backend bytes and the ledger row while the client received
// exactly the bytes it asked for.
//
// The unit test in internal/proxy/object proves the orchestration no longer
// calls delete. This one proves the bytes and the row are still there
// afterwards, against a real backend and a real database, which is where the
// loss actually happened.
func TestIntegrity_RangedReadKeepsHealthyCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	restore := enableVerifyOnRead(t)
	defer restore()

	key := uniqueKey(t, "verify-range")
	body := bytes.Repeat([]byte("R"), 400)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if hashed := queryHashedCopies(t, key); hashed != 1 {
		t.Fatalf("expected the written copy to carry a hash, got %d hashed copies", hashed)
	}
	before := queryObjectBackends(t, key)
	if len(before) != 1 {
		t.Fatalf("expected a single copy so a read cannot fail over, got %v", before)
	}

	// Read a slice and close it, which is what drives verification.
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
		Range:  aws.String("bytes=0-99"),
	})
	if err != nil {
		t.Fatalf("ranged GetObject: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read ranged body: %v", err)
	}
	_ = resp.Body.Close()

	if !bytes.Equal(got, body[:100]) {
		t.Errorf("ranged GET returned %d bytes that do not match the source", len(got))
	}

	// Deletion is asynchronous on the failure path, so a passing assertion has
	// to survive the window in which it would have happened.
	time.Sleep(2 * time.Second)

	victim := before[0]
	if _, err := allBackends[victim].GetObject(ctx, internalKey(key), ""); err != nil {
		t.Errorf("a healthy ranged read destroyed the bytes on %s: %v", victim, err)
	}
	if after := queryObjectBackends(t, key); len(after) != 1 {
		t.Errorf("ledger lists %v after a healthy ranged read, want the single original copy", after)
	}

	// The object must still be wholly readable, which is the property the
	// deletion silently took away.
	full, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("whole-object GetObject after a ranged read: %v", err)
	}
	whole, err := io.ReadAll(full.Body)
	if err != nil {
		t.Fatalf("read whole body: %v", err)
	}
	_ = full.Body.Close()
	if !bytes.Equal(whole, body) {
		t.Error("the object no longer reads back as written after a ranged read")
	}
}

// -------------------------------------------------------------------------
// VERIFY ON REPLICATE
// -------------------------------------------------------------------------

// enableVerifyOnReplicate turns on the replicator's read-back check and returns
// a function restoring the previous configuration. Off by default, so a test
// that wants it has to ask.
func enableVerifyOnReplicate(t *testing.T) func() {
	t.Helper()

	previous := testWorkers.Replicator.IntegrityConfig()
	testWorkers.Replicator.SetIntegrityConfig(&config.IntegrityConfig{
		Enabled:           true,
		VerifyOnReplicate: true,
	})
	return func() { testWorkers.Replicator.SetIntegrityConfig(previous) }
}

// seedHashedObject writes one object and backfills its hash, leaving a single
// copy the replicator can later be pointed at. The write path only stores a
// hash when integrity is configured, so the backfill is what puts a digest on
// the row for verification to compare against.
func seedHashedObject(t *testing.T, ctx context.Context, client *s3.Client, prefix string, body []byte) string {
	t.Helper()

	key := uniqueKey(t, prefix)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, _ = testWorkers.Scrubber.Backfill(ctx, 100, 0, nil); queryHashedCopies(t, key) != 1 {
		t.Fatalf("expected the written copy of %q to carry a hash after backfill", key)
	}
	return key
}

// TestIntegrity_VerifyOnReplicateRejectsACorruptCopy is the point of the
// setting. The source copy is corrupted after its hash is recorded, so the
// replica is a faithful copy of bad bytes: without the read-back it is written,
// recorded, and counts toward the replication factor, leaving the object
// looking twice as durable as it is.
func TestIntegrity_VerifyOnReplicateRejectsACorruptCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	restore := enableVerifyOnReplicate(t)
	defer restore()

	body := bytes.Repeat([]byte("V"), 512)
	key := seedHashedObject(t, ctx, client, "verify-replicate-bad", body)

	origin := queryObjectBackends(t, key)
	if len(origin) != 1 {
		t.Fatalf("expected a single copy before replication, got %v", origin)
	}
	corruptBackendCopy(t, ctx, origin[0], key, bytes.Repeat([]byte("X"), 512))

	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if after := queryObjectBackends(t, key); len(after) != 1 {
		t.Errorf("ledger lists %v; the replica disagreed with its source and must not have been recorded", after)
	}
}

// TestIntegrity_VerifyOnReplicateAdmitsAGoodCopy is the control: verification
// passing must leave ordinary replication alone. Without this, a check that
// rejected everything would look identical to the test above.
func TestIntegrity_VerifyOnReplicateAdmitsAGoodCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	restore := enableVerifyOnReplicate(t)
	defer restore()

	body := bytes.Repeat([]byte("G"), 512)
	key := seedHashedObject(t, ctx, client, "verify-replicate-good", body)

	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if after := queryObjectBackends(t, key); len(after) != 2 {
		t.Fatalf("expected 2 copies after verified replication, got %v", after)
	}
	assertObjectIntact(t, ctx, client, key, body, "after verified replication")
}

// TestIntegrity_VerifyOnReplicateRecordsAnUncheckableCopy pins the arm that
// keeps the setting from costing durability: a source with no stored hash has
// nothing to compare against, and the replica is recorded rather than refused.
// Refusing it would leave every object written before integrity was enabled
// permanently under-replicated.
func TestIntegrity_VerifyOnReplicateRecordsAnUncheckableCopy(t *testing.T) {
	client := newS3Client(t)
	ctx := context.Background()
	resetState(t)

	restore := enableVerifyOnReplicate(t)
	defer restore()

	// No backfill, so the row carries no content_hash.
	key := uniqueKey(t, "verify-replicate-unhashed")
	body := bytes.Repeat([]byte("U"), 512)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if hashed := queryHashedCopies(t, key); hashed != 0 {
		t.Fatalf("expected an unhashed copy, got %d hashed", hashed)
	}

	if _, err := testWorkers.Replicator.Replicate(ctx, replicationFactorTwo(), nil); err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if after := queryObjectBackends(t, key); len(after) != 2 {
		t.Errorf("expected 2 copies, got %v; an unverifiable copy must still be recorded", after)
	}
}
