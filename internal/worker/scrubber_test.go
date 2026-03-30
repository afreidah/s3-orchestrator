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
	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func setupScrubber(t *testing.T) (*Scrubber, *MockScrubberDeps, *backendtest.MockObjectBackend, *mockMetadataStore) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := NewMockScrubberDeps(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}
	s := NewScrubber(ops, nil)
	s.SetConfig(&config.IntegrityConfig{
		Enabled:           true,
		ScrubberBatchSize: 100,
	})
	return s, ops, be, ms
}

func TestScrub_MatchingHash(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)
	body := "hello world"
	expectedHash := hashString(body)

	ms.randomHashedObjects = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: expectedHash},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: 11,
	}, nil)

	checked, failed := s.Scrub(context.Background(), 10)
	if checked != 1 {
		t.Errorf("expected 1 checked, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestScrub_HashMismatch(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "badhash"},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(2)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "bucket/key1", "integrity_scrub_failed", int64(11))
	be.EXPECT().GetObject(gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("hello world")),
		Size: 11,
	}, nil)

	checked, failed := s.Scrub(context.Background(), 10)
	if checked != 1 {
		t.Errorf("expected 1 checked, got %d", checked)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}
}

func TestScrub_BackendError(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)

	ms.randomHashedObjects = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "somehash"},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), "bucket/key1", "").Return(nil, errors.New("backend down"))

	checked, failed := s.Scrub(context.Background(), 10)
	if checked != 0 {
		t.Errorf("expected 0 checked, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestScrub_EmptyBatch(t *testing.T) {
	t.Parallel()
	s, ops, _, ms := setupScrubber(t)
	ms.randomHashedObjects = nil
	ops.EXPECT().Store().Return(ms).AnyTimes()

	checked, failed := s.Scrub(context.Background(), 10)
	if checked != 0 || failed != 0 {
		t.Errorf("expected 0/0, got %d/%d", checked, failed)
	}
}

func TestBackfill_ComputesAndStoresHash(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)
	body := "backfill me"
	expectedHash := hashString(body)

	ms.objectsWithoutHash = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: int64(len(body))},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: int64(len(body)),
	}, nil)

	processed, nextOffset := s.Backfill(context.Background(), 10, 0)
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

func TestBackfill_Pagination(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)

	// Return a full batch to trigger pagination
	locs := make([]store.ObjectLocation, 5)
	for i := range locs {
		locs[i] = store.ObjectLocation{ObjectKey: "bucket/key", BackendName: "b1", SizeBytes: 3}
	}
	ms.objectsWithoutHash = locs
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil).Times(5)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {}).Times(5)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), gomock.Any(), "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader("abc")),
		Size: 3,
	}, nil).Times(5)

	processed, nextOffset := s.Backfill(context.Background(), 5, 0)
	if processed != 5 {
		t.Errorf("expected 5 processed, got %d", processed)
	}
	if nextOffset != 5 {
		t.Errorf("expected nextOffset 5 for full batch, got %d", nextOffset)
	}
}

func TestBackfill_UnencryptedObject(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)
	body := "plaintext object"
	expectedHash := hashString(body)

	ms.objectsWithoutHash = []store.ObjectLocation{
		{ObjectKey: "bucket/plain", BackendName: "b1", SizeBytes: int64(len(body)), Encrypted: false},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), "bucket/plain", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(strings.NewReader(body)),
		Size: int64(len(body)),
	}, nil)

	processed, _ := s.Backfill(context.Background(), 10, 0)
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	if ms.lastUpdatedHash != expectedHash {
		t.Errorf("expected hash %s, got %s", expectedHash, ms.lastUpdatedHash)
	}
}

func TestBackfill_BackendError(t *testing.T) {
	t.Parallel()
	s, ops, be, ms := setupScrubber(t)

	ms.objectsWithoutHash = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 10},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().WithTimeout(gomock.Any()).Return(context.Background(), func() {})
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	be.EXPECT().GetObject(gomock.Any(), "bucket/key1", "").Return(nil, errors.New("timeout"))

	processed, _ := s.Backfill(context.Background(), 10, 0)
	if processed != 0 {
		t.Errorf("expected 0 processed, got %d", processed)
	}
}

func TestBackfill_EmptyBatch(t *testing.T) {
	t.Parallel()
	s, ops, _, ms := setupScrubber(t)
	ms.objectsWithoutHash = nil
	ops.EXPECT().Store().Return(ms).AnyTimes()

	processed, nextOffset := s.Backfill(context.Background(), 10, 0)
	if processed != 0 || nextOffset != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, nextOffset)
	}
}

func TestScrubber_SetConfig(t *testing.T) {
	t.Parallel()
	s := NewScrubber(nil, nil)
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

func TestScrub_ContextCancelled(t *testing.T) {
	t.Parallel()
	s, ops, _, ms := setupScrubber(t)
	ms.randomHashedObjects = []store.ObjectLocation{
		{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: 11, ContentHash: "hash"},
		{ObjectKey: "bucket/key2", BackendName: "b1", SizeBytes: 11, ContentHash: "hash"},
	}
	ops.EXPECT().Store().Return(ms).AnyTimes()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	checked, failed := s.Scrub(ctx, 10)
	if checked != 0 {
		t.Errorf("expected 0 checked with cancelled context, got %d", checked)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

