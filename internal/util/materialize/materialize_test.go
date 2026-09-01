// -------------------------------------------------------------------------------
// Materialize Tests
//
// Author: Alex Freidah
//
// Covers both branches of New / NewEmpty: in-memory for objects up to
// MemThreshold, and the self-unlinking tempfile branch for larger ones. The
// seekable-body invariant is the regression pin for #815 - a non-seekable body
// forces the AWS SDK onto its chunked-TE signing path and breaks OCI with
// HTTP 411 - and, per #972, keeps signed-payload PutObject off io.ReadAll.
// -------------------------------------------------------------------------------

package materialize

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestMemoryBranch covers the small-object path. The sink returns an in-memory
// writer, no tempfile is created, and the resulting body is a seekable
// *bytes.Reader.
func TestMemoryBranch(t *testing.T) {
	t.Parallel()
	mb, err := NewEmpty(int64(MemThreshold))
	if err != nil {
		t.Fatalf("NewEmpty: %v", err)
	}
	defer mb.Cleanup()

	payload := []byte("hello world")
	if _, err := mb.Writer().Write(payload); err != nil {
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

// TestTempfileBranch covers the spill-to-disk path. Above the memory threshold
// the sink writes through an *os.File that has already been unlinked, so the
// file vanishes on Close even if the process crashes between Write and Reader.
// Verifies the file was unlinked at create time (so a leak is impossible).
func TestTempfileBranch(t *testing.T) {
	t.Parallel()
	mb, err := NewEmpty(int64(MemThreshold) + 1)
	if err != nil {
		t.Fatalf("NewEmpty: %v", err)
	}
	t.Cleanup(mb.Cleanup)

	if mb.file == nil {
		t.Fatal("expected tempfile branch, got memory branch")
	}
	if _, err := os.Stat(mb.file.Name()); !os.IsNotExist(err) { //nolint:gosec // G703: path comes from os.CreateTemp, not user input
		t.Errorf("tempfile %q was not unlinked at create time; cleanup invariant broken", mb.file.Name())
	}

	payload := []byte("spillover-payload")
	if _, err := mb.Writer().Write(payload); err != nil {
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
	// The body must be the same *os.File so PutObject sees a seekable reader
	// rather than a bytes.Reader copy.
	if _, ok := body.(*os.File); !ok {
		t.Errorf("tempfile branch returned %T, want *os.File", body)
	}
}

// TestHasherPopulatedDuringMaterialize verifies the optional hasher writes the
// SHA-256 in the same single buffering pass so the body is not re-scanned for
// integrity.
func TestHasherPopulatedDuringMaterialize(t *testing.T) {
	t.Parallel()
	payload := []byte("integrity-checked-body")
	h := sha256.New()
	mb, err := New(bytes.NewReader(payload), int64(len(payload)), h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mb.Cleanup()

	if got := mb.Size(); got != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", got, len(payload))
	}
	want := sha256.Sum256(payload)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Error("hash computed during materialize does not match the payload")
	}
	// The body should still be replayable from offset 0 - hashing must not
	// consume the materialized bytes.
	rdr, err := mb.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	got, _ := io.ReadAll(rdr)
	if !bytes.Equal(got, payload) {
		t.Errorf("body content drifted after hash: got %q want %q", got, payload)
	}
}

// TestReaderResetsBetweenCalls pins that consecutive Reader() calls each start
// at offset 0 so failover attempts replay the body cleanly.
func TestReaderResetsBetweenCalls(t *testing.T) {
	t.Parallel()
	payload := []byte("retry-replay-body")
	mb, err := New(bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
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

// TestSpillDir_LargeBodiesUseTheConfiguredDirectory pins the knob that bounds a
// deployment's memory footprint. The default lands in the OS temp directory,
// which is tmpfs under the systemd default and in most container images, so a
// spill that exists to keep large objects off the heap puts them straight back
// in RAM unless this points somewhere real.
//
// Asserted by pointing it at a directory that does not exist: the spill then
// has to fail, which it can only do if the setting reached os.CreateTemp. The
// file itself is unlinked on creation, so there is nothing on disk to look for.
//
// Not parallel: the spill directory is process-wide.
func TestSpillDir_LargeBodiesUseTheConfiguredDirectory(t *testing.T) {
	SetSpillDir(filepath.Join(t.TempDir(), "no-such-directory"))
	t.Cleanup(func() { SetSpillDir("") })

	if _, err := NewEmpty(int64(MemThreshold) + 1); err == nil {
		t.Error("a body too large for memory should have failed to spill into a missing directory")
	}

	// A body small enough to stay in memory never touches the directory, so it
	// must be unaffected by one that cannot be written to.
	mb, err := NewEmpty(int64(MemThreshold))
	if err != nil {
		t.Fatalf("in-memory body should not consult the spill directory: %v", err)
	}
	mb.Cleanup()
}

// TestSpillDir_EmptyRestoresTheOSDefault holds the reset path, which every test
// that sets the directory relies on for isolation.
func TestSpillDir_EmptyRestoresTheOSDefault(t *testing.T) {
	SetSpillDir(t.TempDir())
	SetSpillDir("")

	if got := spillTarget(); got != "" {
		t.Errorf("spillTarget() = %q, want empty so os.CreateTemp picks the OS default", got)
	}
}
