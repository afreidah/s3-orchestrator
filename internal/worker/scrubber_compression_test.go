// -------------------------------------------------------------------------------
// Scrubber Compression Tests
//
// Author: Alex Freidah
//
// content_hash covers the bytes the client wrote, so a compressed copy has to be
// decoded before it is hashed. Getting this wrong is not a missed verification:
// the scrubber deletes copies it decides are corrupt, so hashing the stored form
// would make it destroy every compressed object it inspected. These tests pin
// the decode, its order against decryption, and the refusal to judge a copy the
// scrubber cannot decode.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// scrubChunk is the codec chunk size these tests encode at, small enough that a
// modest fixture still crosses frame boundaries.
const scrubChunk = compression.MinChunkSize

// newScrubCodec builds a codec and closes it with the test.
func newScrubCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, scrubChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// encodeForScrub returns a compressible body with its stored encoding.
func encodeForScrub(t *testing.T, c *compression.Codec) (plain, stored []byte) {
	t.Helper()
	plain = make([]byte, 0, scrubChunk*2)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for len(plain) < scrubChunk*2 {
		plain = append(plain, line...)
	}
	var buf bytes.Buffer
	if _, err := c.Compress(&buf, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	return plain, buf.Bytes()
}

// compressedRow describes a stored encoding the way a compressed PUT would have
// recorded it.
func compressedRow(hash string, storedSize int) core.ObjectLocation {
	return core.ObjectLocation{
		ObjectKey:                "bucket/key1",
		BackendName:              "b1",
		SizeBytes:                int64(storedSize),
		ContentHash:              hash,
		CompressionAlgorithm:     compression.Algorithm,
		CompressionFormatVersion: compression.FormatVersion,
	}
}

// TestScrub_CompressedObjectVerifies is the headline: a copy whose stored bytes
// are an encoding passes verification against the hash of what the client wrote.
// Before the decode it failed, and the scrubber deleted the copy.
func TestScrub_CompressedObjectVerifies(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)
	codec := newScrubCodec(t)
	s.hasher.codec = codec

	plain, stored := encodeForScrub(t, codec)
	ms.randomHashedObjects = []core.ObjectLocation{compressedRow(hashString(string(plain)), len(stored))}

	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(stored)),
		Size: int64(len(stored)),
	}, func() {}, nil)
	// No DeleteOrEnqueue expectation: a verified copy must not be touched, and
	// the mock fails the test if one arrives.
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	sum := s.Scrub(context.Background(), 10, nil)
	if sum.Attempted != 1 {
		t.Errorf("attempted = %d, want 1", sum.Attempted)
	}
	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0; a compressed copy was judged corrupt", sum.Failed)
	}
}

// TestScrub_CompressedObjectHashBackfill checks the other half of the worker:
// hashing a copy that has no hash yet records the digest of the client's bytes,
// not of the encoding. A wrong digest here is worse than none, because every
// later scrub then reads it as corruption.
func TestScrub_CompressedObjectHashBackfill(t *testing.T) {
	t.Parallel()
	s, ops, _, be, ms := setupScrubber(t)
	codec := newScrubCodec(t)
	s.hasher.codec = codec

	plain, stored := encodeForScrub(t, codec)
	row := compressedRow("", len(stored))
	ms.objectsWithoutHash = []core.ObjectLocation{row}

	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(stored)),
		Size: int64(len(stored)),
	}, func() {}, nil)

	sum, _ := s.Backfill(context.Background(), 10, 0, nil)
	if sum.Succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1", sum.Succeeded)
	}
	if got, want := ms.lastUpdatedHash, hashString(string(plain)); got != want {
		t.Errorf("stored hash = %q, want the digest of the client's bytes %q", got, want)
	}
}

// TestScrub_CompressedWithoutCodecDoesNotJudge pins the refusal: an orchestrator
// with no codec cannot tell whether a compressed copy is good, and a scrubber
// that guesses deletes copies that were never damaged. The read must fail
// instead.
func TestScrub_CompressedWithoutCodecDoesNotJudge(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)
	codec := newScrubCodec(t)
	plain, stored := encodeForScrub(t, codec)
	s.hasher.codec = nil

	ms.randomHashedObjects = []core.ObjectLocation{compressedRow(hashString(string(plain)), len(stored))}

	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(stored)),
		Size: int64(len(stored)),
	}, func() {}, nil)
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	// Skipped rather than failed is the whole point: the scrubber deletes what
	// it judges corrupt, and a copy it could not decode has not been judged.
	sum := s.Scrub(context.Background(), 10, nil)
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", sum.Skipped)
	}
	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0; an unreadable copy is not a corrupt one", sum.Failed)
	}
}

// failingDecoder decodes nothing, standing in for stored bytes that will not
// decode without hand-building a corrupt object.
type failingDecoder struct{ err error }

// DecompressStream implements StreamDecompressor.
func (f failingDecoder) DecompressStream(_ io.Reader) (io.ReadCloser, error) { return nil, f.err }

// TestScrub_UndecodableCopyIsNotCorrupt checks that bytes the codec rejects are
// reported as a read failure rather than as a hash mismatch. The distinction
// decides whether the copy is deleted.
func TestScrub_UndecodableCopyIsNotCorrupt(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)
	s.hasher.codec = failingDecoder{err: errors.New("frame will not decode")}

	ms.randomHashedObjects = []core.ObjectLocation{compressedRow(hashString("whatever"), 64)}

	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(make([]byte, 64))),
		Size: 64,
	}, func() {}, nil)
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	sum := s.Scrub(context.Background(), 10, nil)
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", sum.Skipped)
	}
	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0; bytes that will not decode are not proof of corruption", sum.Failed)
	}
}

// TestScrub_CompressedConfigDisabledStillVerifies checks the decode is driven by
// the row rather than by config: an operator who turns compression off still has
// objects stored compressed, and the scrubber has to keep verifying them.
func TestScrub_CompressedConfigDisabledStillVerifies(t *testing.T) {
	t.Parallel()
	s, ops, pl, be, ms := setupScrubber(t)
	codec := newScrubCodec(t)
	s.hasher.codec = codec
	s.SetConfig(&config.IntegrityConfig{Enabled: true, ScrubberBatchSize: 100})

	plain, stored := encodeForScrub(t, codec)
	ms.randomHashedObjects = []core.ObjectLocation{compressedRow(hashString(string(plain)), len(stored))}

	ops.EXPECT().GetBackend("b1").Return(be, nil)
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(stored)),
		Size: int64(len(stored)),
	}, func() {}, nil)
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	if sum := s.Scrub(context.Background(), 10, nil); sum.Failed != 0 {
		t.Errorf("failed = %d, want 0", sum.Failed)
	}
}
