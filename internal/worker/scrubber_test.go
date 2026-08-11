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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
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

// TestScrub_DiscardedCopyDropsItsLocation verifies a corrupted copy loses its
// ledger row along with its bytes. Leaving the row behind lets the replicator
// keep counting a copy that is gone, so the object stays below its replication
// factor with nothing to trigger a rebuild.
func TestScrub_DiscardedCopyDropsItsLocation(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "expected-hash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(2)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "bucket/key1", "integrity_scrub_failed", int64(11))
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("different bytes")),
		Size: 11,
	}, func() {}, nil)

	if sum := s.Scrub(context.Background(), 10, nil); sum.Failed != 1 {
		t.Fatalf("expected 1 mismatch, got %+v", sum)
	}
	if len(ms.deletedLocations) != 1 || ms.deletedLocations[0] != "bucket/key1@b1" {
		t.Errorf("discarded copy's location not dropped, got %v", ms.deletedLocations)
	}
}

// TestScrub_StampsEveryAttempt verifies the sweep advances past a copy it could
// not read. Stamping only successful verifications would leave a permanently
// broken copy at the head of the queue, starving everything behind it.
func TestScrub_StampsEveryAttempt(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/unreadable", BackendName: "b1", SizeBytes: 11, ContentHash: "abc"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/unreadable", "").
		Return(nil, func() {}, errors.New("backend down"))

	s.Scrub(context.Background(), 10, nil)

	if len(ms.scrubbed) != 1 || ms.scrubbed[0] != "bucket/unreadable@b1" {
		t.Errorf("an unreadable copy must still be stamped, got %v", ms.scrubbed)
	}
}

// TestScrub_ReportsCoverage verifies the cycle publishes how far behind
// verification is, which is the figure operators alert on.
// Deliberately not parallel: the assertions read process-wide gauges that
// every other scrub test overwrites through reportCoverage, so running
// alongside them reads whichever cycle finished last.
func TestScrub_ReportsCoverage(t *testing.T) {
	s, ops, _, _, ms := setupScrubber(t)

	ms.oldestUnverified = 36 * time.Hour
	ms.neverVerified = 42
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	s.Scrub(context.Background(), 10, nil)

	if got := promtest.ToFloat64(telemetry.IntegrityNeverVerifiedCopies); got != 42 {
		t.Errorf("never-verified gauge = %v, want 42", got)
	}
	if got := promtest.ToFloat64(telemetry.IntegrityOldestUnverifiedSeconds); got != (36 * time.Hour).Seconds() {
		t.Errorf("oldest-unverified gauge = %v, want %v", got, (36 * time.Hour).Seconds())
	}
}

// TestScrub_SurvivesBookkeepingFailures verifies the cycle still completes when
// the ledger writes that follow a verification fail. The scrub result is what
// matters; a failed stamp or row removal is logged and retried next sweep
// rather than aborting the batch.
func TestScrub_SurvivesBookkeepingFailures(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "expected-hash"},
	}
	ms.markScrubbedErr = errors.New("stamp failed")
	ms.deleteLocationErr = errors.New("row removal failed")
	ms.oldestUnverifiedErr = errors.New("coverage query failed")

	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(2)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "bucket/key1", "integrity_scrub_failed", int64(11))
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("different bytes")),
		Size: 11,
	}, func() {}, nil)

	if sum := s.Scrub(context.Background(), 10, nil); sum.Failed != 1 {
		t.Errorf("the mismatch must still be reported, got %+v", sum)
	}
}

// -------------------------------------------------------------------------
// REPORTING
// -------------------------------------------------------------------------

// TestScrub_UnreadableCopyIsCountedAndLabelled pins the distinction that a
// silent fleet depends on: a copy the scrubber could not read is reported as
// unreadable, not as a failure and not as a pass.
//
// Counting it as a failure would overstate corruption; leaving it out of the
// summary entirely, which is what "checked N, failed 0" used to do, lets a
// fleet whose copies are all unreadable report a clean pass.
func TestScrub_UnreadableCopyIsCountedAndLabelled(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/gone", BackendName: "b1", SizeBytes: 11, ContentHash: "somehash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/gone", "").
		Return(nil, nil, errors.New("no such key"))

	var statuses []string
	var labels []string
	observer := func(step progress.Step) {
		if step.Phase == progress.PhaseEnd {
			statuses = append(statuses, step.Status)
			labels = append(labels, step.Label)
		}
	}

	sum := s.Scrub(context.Background(), 10, observer)

	if sum.Skipped != 1 {
		t.Errorf("unreadable copy: Skipped = %d, want 1 (%+v)", sum.Skipped, sum)
	}
	if sum.Failed != 0 {
		t.Errorf("unreadable copy must not count as a hash failure, got Failed = %d", sum.Failed)
	}
	if len(statuses) != 1 || statuses[0] != progress.StatusUnreadable {
		t.Errorf("reported status = %v, want [%s]", statuses, progress.StatusUnreadable)
	}
	// The backend belongs in the label because copies are scrubbed per
	// (key, backend); without it a replicated object reads as a repeated line.
	if len(labels) != 1 || labels[0] != "bucket/gone [b1]" {
		t.Errorf("progress label = %v, want [\"bucket/gone [b1]\"]", labels)
	}
}

// TestScrub_MismatchStaysDistinctFromUnreadable guards the other side of the
// split: a copy that was read and did not match is a failure, so widening the
// unreadable bucket cannot quietly swallow real corruption.
func TestScrub_MismatchStaysDistinctFromUnreadable(t *testing.T) {
	t.Parallel()
	s, ops, placement, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/rotted", BackendName: "b1", SizeBytes: 5, ContentHash: "not-the-hash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/rotted", "").
		Return(&backend.GetObjectResult{
			Body: io.NopCloser(strings.NewReader("hello")),
			Size: 5,
		}, func() {}, nil)
	placement.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), "b1", "bucket/rotted", gomock.Any(), gomock.Any()).AnyTimes()

	var statuses []string
	sum := s.Scrub(context.Background(), 10, func(step progress.Step) {
		if step.Phase == progress.PhaseEnd {
			statuses = append(statuses, step.Status)
		}
	})

	if sum.Failed != 1 {
		t.Errorf("hash mismatch: Failed = %d, want 1 (%+v)", sum.Failed, sum)
	}
	if sum.Skipped != 0 {
		t.Errorf("hash mismatch must not be reported as unreadable, got Skipped = %d", sum.Skipped)
	}
	if len(statuses) != 1 || statuses[0] == progress.StatusUnreadable {
		t.Errorf("reported status = %v, want a mismatch status", statuses)
	}
}

// TestScrubCycle_LogsAPassOfOnlyUnreadableCopies guards the quietest failure
// mode. Skipped is excluded from Attempted, so a cycle whose every copy was
// unreadable reports zero checked and zero failed. Gating the log on those two
// alone meant the tick that most needed reporting produced no output at all.
func TestScrubCycle_LogsAPassOfOnlyUnreadableCopies(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	s.SetConfig(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 10})

	ms.randomHashedObjects = []core.ObjectLocation{
		{ObjectKey: "bucket/gone", BackendName: "b1", SizeBytes: 11, ContentHash: "somehash"},
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/gone", "").
		Return(nil, nil, errors.New("no such key"))

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := scrubCycle(context.Background(), s, log); err != nil {
		t.Fatalf("scrubCycle: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "scrub completed") {
		t.Fatalf("a cycle of only unreadable copies logged nothing: %q", out)
	}
	if !strings.Contains(out, `"unreadable":1`) {
		t.Errorf("log line does not report the unreadable count: %s", out)
	}
}

// TestScrubCycle_SilentWhenNothingToDo keeps the log honest in the other
// direction: an empty batch is not worth a line every tick.
func TestScrubCycle_SilentWhenNothingToDo(t *testing.T) {
	t.Parallel()
	s, _, _, _, ms := setupScrubber(t)
	s.SetConfig(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 10})
	ms.randomHashedObjects = nil

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := scrubCycle(context.Background(), s, log); err != nil {
		t.Fatalf("scrubCycle: %v", err)
	}
	if strings.Contains(buf.String(), "scrub completed") {
		t.Errorf("empty cycle should not log a completion line: %s", buf.String())
	}
}
