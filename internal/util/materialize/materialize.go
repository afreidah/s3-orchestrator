// -------------------------------------------------------------------------------
// Materialize - Memory-or-Tempfile Seekable Source
//
// Author: Alex Freidah
//
// Buffers an incoming stream into a seekable form (memory below MemThreshold, a
// self-unlinking tempfile above) so callers can re-read the body without
// scaling heap with object size. Reader-reset and lifecycle semantics are
// documented on the methods below.
// -------------------------------------------------------------------------------

package materialize

import (
	"bytes"
	"fmt"
	"hash"
	"io"
	"os"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// MemThreshold is the largest payload size kept entirely in memory before
// the sink spills to a tempfile. Sized to match the AWS SDK's own internal
// heuristic for the PUT signing path. Not a config knob because the choice
// is an implementation detail, not an operator concern.
const MemThreshold = 32 * 1024 * 1024

// spillDir is where a payload too large for memory is written, or empty for
// the OS temp directory. The default /tmp is often tmpfs, which puts spilled
// objects back in RAM, so operators should point this at real disk. It is
// atomic because every goroutine serving a PUT reads it.
var spillDir atomic.Pointer[string]

// SetSpillDir points large-payload spills at dir. An empty string restores the
// OS temp directory. Call during startup, before serving.
func SetSpillDir(dir string) {
	if dir == "" {
		spillDir.Store(nil)
		return
	}
	spillDir.Store(&dir)
}

// spillTarget returns the directory to create tempfiles in, empty meaning the
// OS default, which is what os.CreateTemp already interprets that way.
func spillTarget() string {
	if d := spillDir.Load(); d != nil {
		return *d
	}
	return ""
}

// Body holds a payload buffered into memory or onto disk, and serves
// io.Readers positioned at offset 0 on each call. The caller invokes Cleanup
// once the payload is no longer needed (always safe to defer, even on a
// materialization error).
type Body struct {
	buf  *bytes.Buffer
	file *os.File
	size int64
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// New copies src into a memory buffer or a tempfile based on size, tee'ing
// the bytes into each supplied hasher in the same pass. Nil hashers are
// skipped. The caller must defer (*Body).Cleanup on the returned body.
func New(src io.Reader, size int64, hashers ...hash.Hash) (*Body, error) {
	b, err := NewEmpty(size)
	if err != nil {
		return nil, err
	}
	w := b.Writer()
	for _, h := range hashers {
		if h != nil {
			w = io.MultiWriter(w, h)
		}
	}
	if _, err := bufpool.Copy(w, src); err != nil {
		b.Cleanup()
		return nil, err
	}
	return b, nil
}

// NewEmpty allocates the underlying sink without writing any bytes. Exposed
// for code paths that want to drive the write themselves (e.g. materializing
// a foreign GetObject body where the integrity hashing is owned by a
// different layer).
func NewEmpty(size int64) (*Body, error) {
	if size <= MemThreshold {
		return &Body{buf: &bytes.Buffer{}}, nil
	}
	f, err := os.CreateTemp(spillTarget(), "s3o-put-*")
	if err != nil {
		return nil, fmt.Errorf("create materialize tempfile: %w", err)
	}
	// Unlink immediately so the file disappears on Close or process exit;
	// Cleanup only needs to Close the fd. Removes the leak window if the
	// process panics mid-write.
	_ = os.Remove(f.Name()) //nolint:gosec // G703: path comes from os.CreateTemp, not user input
	return &Body{file: f}, nil
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Cleanup removes the tempfile when the body spilled to disk. Always safe to
// defer.
func (b *Body) Cleanup() {
	if b.file != nil {
		_ = b.file.Close()
	}
}

// Writer returns the io.Writer a NewEmpty body is filled through. It counts the
// bytes written, since Reader needs the length.
func (b *Body) Writer() io.Writer {
	if b.file != nil {
		return &countingWriter{dst: b.file, written: &b.size}
	}
	return b.buf
}

// countingWriter forwards to the sink and accumulates the byte count.
type countingWriter struct {
	dst     io.Writer
	written *int64
}

// Write forwards and adds what was accepted.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	*c.written += int64(n)
	return n, err
}

// Size returns the number of bytes written to the sink. For the in-memory
// sink this is len(buf); for the tempfile sink this is the byte count
// returned by the copy.
func (b *Body) Size() int64 {
	if b.file != nil {
		return b.size
	}
	return int64(b.buf.Len())
}

// Reader returns a new io.ReadSeeker at offset 0, independent of any other, so
// concurrent uploads can read one payload. The tempfile form returns a section
// reader because a shared *os.File has a single offset.
func (b *Body) Reader() (io.ReadSeeker, error) {
	if b.file != nil {
		return io.NewSectionReader(b.file, 0, b.size), nil
	}
	return bytes.NewReader(b.buf.Bytes()), nil
}
