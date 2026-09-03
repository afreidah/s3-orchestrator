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
// the OS temp directory.
//
// Where the spill lands is very much an operator concern, unlike the threshold
// above: the default is /tmp, which is tmpfs under the systemd default and in
// most container images, so the spill that exists to keep large objects off the
// heap puts them straight back in RAM. Pointing this at real disk is the only
// way to bound the footprint of a fleet-wide pass.
//
// Atomic because it is read from every goroutine serving a PUT. Written once
// during startup, before any of them exist, but a value only safe because of
// when it happens to be written is what this pattern exists to avoid.
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
// the bytes into each supplied hasher so every digest the caller needs comes
// out of the same single pass instead of re-scanning the materialized body.
// A PUT wants two - the ETag's MD5 always, the integrity SHA-256 when it is
// enabled - and nil entries are skipped so a caller can pass an optional
// hasher without branching. The caller must defer (*Body).Cleanup on the
// returned body so the tempfile fd is released when the body is no longer
// needed (safe even on the in-memory branch).
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
	n, err := bufpool.Copy(w, src)
	if err != nil {
		b.Cleanup()
		return nil, err
	}
	b.size = n
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

// Cleanup releases the underlying tempfile when the sink spilled to disk; the
// in-memory branch has no fd to release and the buffer is reclaimed by the GC
// when the Body goes out of scope. Always safe to defer regardless of which
// branch backs the body.
func (b *Body) Cleanup() {
	if b.file != nil {
		_ = b.file.Close()
	}
}

// Writer returns the io.Writer the caller streams source bytes into. Only
// meaningful when NewEmpty was used to construct the sink; New owns its own
// write loop.
func (b *Body) Writer() io.Writer {
	if b.file != nil {
		return b.file
	}
	return b.buf
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

// Reader returns a fresh io.ReadSeeker positioned at offset 0. Safe to call
// repeatedly so failover attempts and encryption layers can each consume the
// body independently. The in-memory variant returns a fresh *bytes.Reader;
// the tempfile variant rewinds the underlying file before returning.
func (b *Body) Reader() (io.ReadSeeker, error) {
	if b.file != nil {
		if _, err := b.file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind materialize tempfile: %w", err)
		}
		return b.file, nil
	}
	return bytes.NewReader(b.buf.Bytes()), nil
}
