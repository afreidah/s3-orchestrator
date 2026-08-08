// -------------------------------------------------------------------------------
// Integrity Scrubber Tests
//
// Author: Alex Freidah
//
// Covers the scrubber's verify-and-repair path: hash matches accept the
// copy, mismatches enqueue cleanup with reason integrity_scrub_failed,
// backend errors are tolerated and counted, empty batches no-op, and the
// backfill path computes-then-stores hashes for objects predating the
// content_hash column. The mismatch enqueue is the load-bearing assertion
// because it is how silent corruption gets surfaced.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"go.uber.org/mock/gomock"
)

// hashString reports whether h string.
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// setupScrubber sets up scrubber.
func setupScrubber(t *testing.T) (*Scrubber, *MockScrubberOps, *MockPlacement, *backendtest.MockObjectBackend, *mockMetadataStore) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := NewMockScrubberOps(ctrl)
	pl := NewMockPlacement(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}
	s := NewScrubber(ScrubberDeps{Ops: ops, Placement: pl, Store: ms})
	s.SetConfig(&config.IntegrityConfig{
		Enabled:           true,
		ScrubberBatchSize: 100,
	})
	return s, ops, pl, be, ms
}

// TestScrub_MatchingHash verifies the scrub matching hash contract.
// Asserts that expected 1 checked, got.
func TestScrub_MatchingHash(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	body := "hello world"
	expectedHash := hashString(body)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: expectedHash},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: 11,
	}, func() {}, nil)

	scrubSum := s.Scrub(context.Background(), 10, nil)
	checked, failed := scrubSum.Attempted, scrubSum.Failed
	if checked != 1 {
		t.Errorf("expected 1 checked, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

// TestScrub_HashMismatch verifies the scrub hash mismatch contract.
// Asserts that expected 1 checked, got.
func TestScrub_HashMismatch(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "badhash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(2)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "bucket/key1", "integrity_scrub_failed", int64(11))
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("hello world")),
		Size: 11,
	}, func() {}, nil)

	scrubSum := s.Scrub(context.Background(), 10, nil)
	checked, failed := scrubSum.Attempted, scrubSum.Failed
	if checked != 1 {
		t.Errorf("expected 1 checked, got %d", checked)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
}

// TestScrub_BackendError verifies the scrub backend error contract.
// Asserts that expected 0 checked, got.
func TestScrub_BackendError(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "somehash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(nil, nil, errors.New("backend down"))

	scrubSum := s.Scrub(context.Background(), 10, nil)
	checked, failed := scrubSum.Attempted, scrubSum.Failed
	if checked != 0 {
		t.Errorf("expected 0 checked, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

// TestScrub_EmptyBatch verifies the scrub empty batch contract.
// Asserts that expected 0/0, got /.
func TestScrub_EmptyBatch(t *testing.T) {
	t.Parallel()
	s, _, _, _, ms := setupScrubber(t)
	ms.randomHashedObjects = nil

	scrubSum := s.Scrub(context.Background(), 10, nil)
	checked, failed := scrubSum.Attempted, scrubSum.Failed
	if checked != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", checked, failed)
	}
}

// TestBackfill_ComputesAndStoresHash verifies the backfill computes and stores hash contract.
// Asserts that expected 1 processed, got.
func TestBackfill_ComputesAndStoresHash(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	body := "backfill me"
	expectedHash := hashString(body)

	ms.objectsWithoutHash = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: int64(len(body))},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: int64(len(body)),
	}, func() {}, nil)

	backfillSum, nextOffset := s.Backfill(context.Background(), 10, 0, nil)
	processed := backfillSum.Succeeded
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	if nextOffset != 0 {
		t.Errorf("expected nextOffset 0, got %d", nextOffset)
	}
	if ms.lastUpdatedHash != expectedHash {
		t.Errorf("expected hash %s, got %s", expectedHash, ms.lastUpdatedHash)
	}
}

// TestBackfill_Pagination verifies the backfill pagination contract.
// Asserts that expected 5 processed, got.
func TestBackfill_Pagination(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)

	// Return a full batch to trigger pagination
	locs := make([]core.ObjectLocation, 5)
	for i := range locs {
		locs[i] = core.ObjectLocation{ObjectKey: "bucket/key", BackendName: "b1", SizeBytes: 3}
	}
	ms.objectsWithoutHash = locs
	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(5)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), gomock.Any(), "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("abc")),
		Size: 3,
	}, func() {}, nil).Times(5)

	backfillSum, nextOffset := s.Backfill(context.Background(), 5, 0, nil)
	processed := backfillSum.Succeeded
	if processed != 5 {
		t.Errorf("expected 5 processed, got %d", processed)
	}
	if nextOffset != 5 {
		t.Errorf("expected nextOffset 5 for full batch, got %d", nextOffset)
	}
}

// TestBackfill_UnencryptedObject verifies the backfill unencrypted object contract.
// Asserts that expected 1 processed, got.
func TestBackfill_UnencryptedObject(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	body := "plaintext object"
	expectedHash := hashString(body)

	ms.objectsWithoutHash = []core.ObjectLocation{
		{ObjectKey: "bucket/plain", BackendName: "b1", SizeBytes: int64(len(body)), Encrypted: false},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/plain", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: int64(len(body)),
	}, func() {}, nil)

	backfillSum, _ := s.Backfill(context.Background(), 10, 0, nil)
	processed := backfillSum.Succeeded
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	if ms.lastUpdatedHash != expectedHash {
		t.Errorf("expected hash %s, got %s", expectedHash, ms.lastUpdatedHash)
	}
}

// TestBackfill_BackendError verifies the backfill backend error contract.
// Asserts that expected 0 processed, got.
func TestBackfill_BackendError(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)

	ms.objectsWithoutHash = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 10},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(nil, nil, errors.New("timeout"))

	backfillSum, _ := s.Backfill(context.Background(), 10, 0, nil)
	processed := backfillSum.Succeeded
	if processed != 0 {
		t.Errorf("expected 0 processed, got %d", processed)
	}
}

// TestBackfill_EmptyBatch verifies the backfill empty batch contract.
// Asserts that expected 0/0, got /.
func TestBackfill_EmptyBatch(t *testing.T) {
	t.Parallel()
	s, _, _, _, ms := setupScrubber(t)
	ms.objectsWithoutHash = nil

	backfillSum, nextOffset := s.Backfill(context.Background(), 10, 0, nil)
	processed := backfillSum.Succeeded
	if processed != 0 || nextOffset != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, nextOffset)
	}
}

// TestScrubber_SetConfig verifies the scrubber set config contract.
// Asserts that expected batch size 50, got.
func TestScrubber_SetConfig(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	s := NewScrubber(ScrubberDeps{Ops: NewMockScrubberOps(ctrl), Placement: NewMockPlacement(ctrl), Store: &mockMetadataStore{}})
	if s.Config() != nil {
		t.Fatal("expected nil config initially")
	}
	cfg := &config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 50}
	s.SetConfig(cfg)
	got := s.Config()
	if got == nil || got.ScrubberBatchSize != 50 {
		t.Errorf("expected batch size 50, got %v", got)
	}
}

// TestScrub_ContextCancelled verifies the scrub context cancelled contract.
// Asserts that expected 0 checked with cancelled context, got.
func TestScrub_ContextCancelled(t *testing.T) {
	t.Parallel()
	s, _, _, _, ms := setupScrubber(t)
	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "hash"},
		{ObjectKey: "bucket/key2", BackendName: "b1", SizeBytes: 11, ContentHash: "hash"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	scrubSum := s.Scrub(ctx, 10, nil)
	checked, failed := scrubSum.Attempted, scrubSum.Failed
	if checked != 0 {
		t.Errorf("expected 0 checked with cancelled context, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

// TestScrub_RefusesEnvelopeOnPlainRow verifies the scrubber will not hash an
// envelope as if it were plaintext. Doing so would write a ciphertext digest
// into content_hash, making the divergence look verified and turning any later
// repair of the flag into a false integrity failure. The copy must be skipped,
// not counted as checked, and above all not enqueued for deletion.
func TestScrub_RefusesEnvelopeOnPlainRow(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	ciphertext := "SENC\x01" + strings.Repeat("x", 64)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: int64(len(ciphertext)), ContentHash: hashString(ciphertext)},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(ciphertext)),
		Size: int64(len(ciphertext)),
	}, func() {}, nil)

	scrubSum := s.Scrub(context.Background(), 10, nil)
	if scrubSum.Attempted != 0 {
		t.Errorf("a divergent copy must not count as checked, got %d", scrubSum.Attempted)
	}
	if scrubSum.Failed != 0 {
		t.Errorf("a divergent copy is skipped, not failed, got %d", scrubSum.Failed)
	}
}

// TestBackfill_RefusesEnvelopeOnPlainRow verifies backfill never writes a hash
// for a copy whose bytes disagree with its row. This is the path that would
// cement the divergence permanently, since these rows have no hash yet.
func TestBackfill_RefusesEnvelopeOnPlainRow(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	ciphertext := "SENC\x01" + strings.Repeat("x", 64)

	ms.objectsWithoutHash = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: int64(len(ciphertext))},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(ciphertext)),
		Size: int64(len(ciphertext)),
	}, func() {}, nil)

	sum, _ := s.Backfill(context.Background(), 10, 0, nil)
	if sum.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", sum.Failed)
	}
	if ms.lastUpdatedHash != "" {
		t.Errorf("no hash may be stored for a divergent copy, got %q", ms.lastUpdatedHash)
	}
}

// TestScrub_RefusesContradictoryRow verifies a row claiming encryption without
// a key is rejected before any backend read, since no plaintext hash can be
// computed from it and spending a read to discover that is waste.
func TestScrub_RefusesContradictoryRow(t *testing.T) {
	t.Parallel()
	s, ops, _, _, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 100, ContentHash: "abc", Encrypted: true},
	}
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	scrubSum := s.Scrub(context.Background(), 10, nil)
	if scrubSum.Attempted != 0 {
		t.Errorf("a contradictory row must not count as checked, got %d", scrubSum.Attempted)
	}
}
