// -------------------------------------------------------------------------------
// CopyObject Source Materialization Tests
//
// Author: Alex Freidah
//
// Covers both branches of newCopyMaterializeSink: in-memory for objects
// up to copyMaterializeMemThreshold, and the self-unlinking tempfile
// branch for larger ones. The seekable-body invariant is the regression
// pin for #815  -  a non-seekable body forces the AWS SDK onto its
// chunked-TE signing path and breaks OCI with HTTP 411.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// TestCopyMaterializeSink_MemoryBranch covers the small-object path.
// The sink returns an in-memory writer, no tempfile is created, and
// the resulting body is a seekable bytes.Reader.
func TestCopyMaterializeSink_MemoryBranch(t *testing.T) {
	t.Parallel()
	sink, cleanup, err := newCopyMaterializeSink(int64(copyMaterializeMemThreshold))
	if err != nil {
		t.Fatalf("newCopyMaterializeSink: %v", err)
	}
	defer cleanup()

	payload := []byte("hello world")
	if _, err := sink.writer().Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := sink.seekableBody()
	if err != nil {
		t.Fatalf("seekableBody: %v", err)
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

// TestCopyMaterializeSink_TempfileBranch covers the spill-to-disk
// path. Above the memory threshold the sink writes through an *os.File
// that has already been unlinked, so the file vanishes on Close even
// if the process crashes between Write and seekableBody. Verifies the
// file was unlinked at create time (so a leak is impossible).
func TestCopyMaterializeSink_TempfileBranch(t *testing.T) {
	t.Parallel()
	sink, cleanup, err := newCopyMaterializeSink(int64(copyMaterializeMemThreshold) + 1)
	if err != nil {
		t.Fatalf("newCopyMaterializeSink: %v", err)
	}
	t.Cleanup(cleanup)

	if sink.file == nil {
		t.Fatal("expected tempfile branch, got memory branch")
	}
	if _, err := os.Stat(sink.file.Name()); !os.IsNotExist(err) { //nolint:gosec // G703: path comes from os.CreateTemp, not user input
		t.Errorf("tempfile %q was not unlinked at create time; cleanup invariant broken", sink.file.Name())
	}

	payload := []byte("spillover-payload")
	if _, err := sink.writer().Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := sink.seekableBody()
	if err != nil {
		t.Fatalf("seekableBody: %v", err)
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
