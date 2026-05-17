// -------------------------------------------------------------------------------
// MaterializedBody Tests
//
// Author: Alex Freidah
//
// Covers both branches of newMaterializedBody / emptyMaterializedBody:
// in-memory for objects up to materializeMemThreshold, and the
// self-unlinking tempfile branch for larger ones. The seekable-body
// invariant is the regression pin for #815  -  a non-seekable body
// forces the AWS SDK onto its chunked-TE signing path and breaks OCI
// with HTTP 411.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// TestMaterializedBody_MemoryBranch covers the small-object path. The
// sink returns an in-memory writer, no tempfile is created, and the
// resulting body is a seekable *bytes.Reader.
func TestMaterializedBody_MemoryBranch(t *testing.T) {
	t.Parallel()
	mb, err := emptyMaterializedBody(int64(materializeMemThreshold))
	if err != nil {
		t.Fatalf("emptyMaterializedBody: %v", err)
	}
	defer mb.Cleanup()

	payload := []byte("hello world")
	if _, err := mb.writer().Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := mb.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		t.Errorf("Seek: %v", err)
	}
}

// TestMaterializedBody_TempfileBranch covers the spill-to-disk path.
// Above the memory threshold the sink writes through an *os.File that
// has already been unlinked, so the file vanishes on Close even if the
// process crashes between Write and Reader. Verifies the file was
// unlinked at create time (so a leak is impossible).
func TestMaterializedBody_TempfileBranch(t *testing.T) {
	t.Parallel()
	mb, err := emptyMaterializedBody(int64(materializeMemThreshold) + 1)
	if err != nil {
		t.Fatalf("emptyMaterializedBody: %v", err)
	}
	t.Cleanup(mb.Cleanup)

	if mb.file == nil {
		t.Fatal("expected tempfile branch, got memory branch")
	}
	if _, err := os.Stat(mb.file.Name()); !os.IsNotExist(err) { //nolint:gosec // G703: path comes from os.CreateTemp, not user input
		t.Errorf("tempfile %q was not unlinked at create time; cleanup invariant broken", mb.file.Name())
	}

	payload := []byte("spillover-payload")
	if _, err := mb.writer().Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := mb.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
	// The body must be the same *os.File so PutObject sees a seekable
	// reader rather than a bytes.Reader copy.
	if _, ok := body.(*os.File); !ok {
		t.Errorf("tempfile branch returned %T, want *os.File", body)
	}
}

// TestMaterializedBody_HasherPopulatedDuringMaterialize verifies the
// optional hasher writes the SHA-256 in the same single buffering pass
// so the body is not re-scanned for integrity.
func TestMaterializedBody_HasherPopulatedDuringMaterialize(t *testing.T) {
	t.Parallel()
	payload := []byte("integrity-checked-body")
	h := newSHA256()
	mb, err := newMaterializedBody(bytes.NewReader(payload), int64(len(payload)), h)
	if err != nil {
		t.Fatalf("newMaterializedBody: %v", err)
	}
	defer mb.Cleanup()

	if got := mb.Size(); got != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", got, len(payload))
	}
	if sha256Hex(h) == "" {
		t.Error("expected non-empty hash after materialize")
	}
	// The body should still be replayable from offset 0  -  hashing
	// must not consume the materialized bytes.
	rdr, err := mb.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got, _ := io.ReadAll(rdr)
	if !bytes.Equal(got, payload) {
		t.Errorf("body content drifted after hash: got %q want %q", got, payload)
	}
}

// TestMaterializedBody_ReaderResetsBetweenCalls pins that consecutive
// Reader() calls each start at offset 0 so failover attempts replay
// the body cleanly.
func TestMaterializedBody_ReaderResetsBetweenCalls(t *testing.T) {
	t.Parallel()
	payload := []byte("retry-replay-body")
	mb, err := newMaterializedBody(bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("newMaterializedBody: %v", err)
	}
	defer mb.Cleanup()

	for attempt := range 3 {
		rdr, err := mb.Reader()
		if err != nil {
			t.Fatalf("attempt %d Reader: %v", attempt, err)
		}
		got, _ := io.ReadAll(rdr)
		if !bytes.Equal(got, payload) {
			t.Errorf("attempt %d: got %q want %q", attempt, got, payload)
		}
	}
}
